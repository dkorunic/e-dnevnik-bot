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
)

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

// parseFirstDateTime attempts to parse a timestamp string using a list of layouts.
//
// It takes a slice of layout strings and a timestamp value string. The function
// trims any leading or trailing whitespace from the value and iterates over the
// provided layouts, trying to parse the value using each one. If a layout
// successfully parses the value, the parsed time.Time object is returned.
// If none of the layouts can parse the value, the function returns the zero
// value of time.Time and an error indicating the failure to parse the timestamp.
//
// Layouts that contain the Go timezone directive "07" (e.g. Z0700, -0700) are
// parsed with time.Parse so the embedded offset is honoured. All other layouts
// lack timezone information; they are parsed with time.ParseInLocation using
// time.UTC so the dedup hash and the displayed exam date stay stable across
// hosts in different timezones. Parsing in time.Local would shift all-day
// events by up to ±14 h relative to a server in another zone, changing the
// hash key and showing the wrong calendar day to the user.
func parseFirstDateTime(layouts []string, value string) (time.Time, error) {
	value = strings.TrimSpace(value)

	for _, layout := range layouts {
		var (
			dt  time.Time
			err error
		)

		if strings.Contains(layout, "07") {
			dt, err = time.Parse(layout, value)
		} else {
			dt, err = time.ParseInLocation(layout, value, time.UTC)
		}

		if err == nil {
			return dt, nil
		}
	}

	return time.Time{}, fmt.Errorf("%w: %v", ErrParseTimestamp, value)
}
