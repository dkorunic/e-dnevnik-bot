// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/jordic/goics"
)

const (
	EventDateStart   = "DTSTART"
	EventDescription = "DESCRIPTION"
	EventSummary     = "SUMMARY"

	LayoutISO8601CompactZ    = "20060102T150405Z0700"
	LayoutISO8601CompactNoTZ = "20060102T150405"
	LayoutISO8601Short       = "20060102"
)

var (
	ErrParseTimestamp = errors.New("failed to parse timestamp")

	dateLayouts = []string{
		LayoutISO8601CompactZ,
		LayoutISO8601CompactNoTZ,
		LayoutISO8601Short,
	}

	// layoutParser maps each known layout to the parse function appropriate
	// for it. Layouts that embed timezone information (Z, ±HHMM) use
	// time.Parse so the embedded offset is honoured; layouts without
	// timezone information use parseInUTC so the parsed time is anchored to
	// UTC regardless of the host timezone — required for dedup hash
	// stability across servers in different zones. Adding a new layout
	// means registering its parser here.
	layoutParser = map[string]func(layout, value string) (time.Time, error){
		LayoutISO8601CompactZ:    time.Parse,
		LayoutISO8601CompactNoTZ: parseInUTC,
		LayoutISO8601Short:       parseInUTC,
	}
)

// parseInUTC wraps time.ParseInLocation with time.UTC so a layout without
// timezone information yields a UTC time, not host-local.
func parseInUTC(layout, value string) (time.Time, error) {
	return time.ParseInLocation(layout, value, time.UTC)
}

// ConsumeICal is a ICS data decoder that extracts DTSTART, DESCRIPTION and SUMMARY values, parsing timestamp with
// maximum flexibility and in local timezone, returning optional error.
func (e *Events) ConsumeICal(c *goics.Calendar, _ error) error {
	for _, el := range c.Events {
		node := el.Data

		if node == nil || node[EventDateStart] == nil || node[EventDescription] == nil || node[EventSummary] == nil {
			logger.Debug().Msgf("invalid ICAL data for event: %v", el)

			continue
		}

		timestamp := node[EventDateStart]

		dtstart, err := parseFirstDateTime(dateLayouts, timestamp.Val)
		if err != nil {
			logger.Debug().Msgf("failed to parse event date %v: %v", timestamp.Val, err)

			continue
		}

		if dtstart.IsZero() {
			logger.Debug().Msgf("failed to parse event date %v", timestamp.Val)

			continue
		}

		d := Event{
			Start:       dtstart,
			Description: node[EventDescription].Val,
			Summary:     node[EventSummary].Val,
		}
		*e = append(*e, d)
	}

	return nil
}

// parseFirstDateTime attempts to parse a timestamp string using a list of
// layouts, returning the first successful parse. Each layout is dispatched
// through layoutParser to pick the right time.Parse / time.ParseInLocation
// variant — see the layoutParser comment for why this matters for dedup
// stability. Unregistered layouts default to UTC parsing for the same reason.
func parseFirstDateTime(layouts []string, value string) (time.Time, error) {
	value = strings.TrimSpace(value)

	for _, layout := range layouts {
		parse, ok := layoutParser[layout]
		if !ok {
			parse = parseInUTC
		}

		if dt, err := parse(layout, value); err == nil {
			return dt, nil
		}
	}

	return time.Time{}, fmt.Errorf("%w: %v", ErrParseTimestamp, value)
}
