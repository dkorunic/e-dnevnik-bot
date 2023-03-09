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
	"github.com/araddon/dateparse"
	"github.com/jordic/goics"
)

const (
	EventDateStart   = "DTSTART"
	EventDescription = "DESCRIPTION"
	EventSummary     = "SUMMARY"
)

// ConsumeICal is a ICS data decoder that extracts DTSTART, DESCRIPTION and SUMMARY values, parsing timestamp with
// maximum flexibility and in local timezone, returning optional error.
func (e *Events) ConsumeICal(c *goics.Calendar, err error) error {
	for _, el := range c.Events {
		node := el.Data

		dtstart, err := dateparse.ParseLocal(node[EventDateStart].Val)
		if err != nil {
			return err
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
