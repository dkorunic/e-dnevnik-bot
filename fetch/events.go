// @license
// Copyright (C) 2022  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package fetch

import (
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

		dtstart, err := parseFirstDateTime([]string{
			LayoutISO8601CompactZ,
			LayoutISO8601CompactNoTZ,
			LayoutISO8601Short,
		},
			timestamp.Val)
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
func parseFirstDateTime(layouts []string, value string) (time.Time, error) {
	value = strings.TrimSpace(value)

	for _, layout := range layouts {
		if dt, err := time.Parse(layout, value); err == nil {
			return dt, nil
		}
	}

	return time.Time{}, fmt.Errorf("failed to parse timestamp %v", value)
}
