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

package scrape

import (
	"strings"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/dkorunic/e-dnevnik-bot/fetch"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"

	"github.com/PuerkitoBio/goquery"
)

const (
	TimeFormat       = "02.01.2006."  // DD.MM.YYYY. format
	DateDescription  = "Datum ispita" // exam date field description
	EventSummary     = "Predmet"      // exam summary field description (typically a subject name)
	EventDescription = "Napomena"     // exam remark field description (typically a target of the exam)
)

// parseGrades extracts grades per subject from raw string (grade scrape response body) and grade descriptions,
// constructs grade messages and sends them a message channel, optionally returning an error.
func parseGrades(msgPool *sync.Pool, ch chan<- *msgtypes.Message, username, rawGrades string) error {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawGrades))
	if err != nil {
		return err
	}
	var parsedGrades int

	// each subject has a div with class "table-container new-grades-table"
	doc.Find("div.content > div.table-container.new-grades-table").
		Each(func(i int, table *goquery.Selection) {
			// subject name is in data-action-id attribute
			subject, subjectOK := table.Attr("data-action-id")
			if !subjectOK {
				return
			}

			var descriptions []string
			// row descriptions are in div with class "flex-table header" in each span
			table.Find("div.flex-table.header > div.flex-row > span").
				Each(func(j int, column *goquery.Selection) {
					txt := strings.TrimSpace(column.Text())
					descriptions = append(descriptions, txt)
				})

			// grades are in each div with class "flex-table row" ...
			table.Find("div.flex-table.row").
				Each(func(j int, row *goquery.Selection) {
					var spans []string

					// ... and in each span
					row.Find("div.flex-row > span").
						Each(func(k int, column *goquery.Selection) {
							// clean excess whitespace and newlines
							txt := strings.TrimSpace(column.Text())
							if len(txt) > 0 {
								txt = strings.ReplaceAll(txt, "\n", " ")
								txt = strings.Join(strings.Fields(txt), " ")
							}
							spans = append(spans, txt)
						})

					// once we have a single grade with all required fields, send it through the channel
					m := msgPool.Get().(*msgtypes.Message) //nolint:forcetypeassert
					m.Username = username
					m.Subject = subject
					m.Descriptions = descriptions
					m.Fields = spans
					ch <- m
					parsedGrades++
				})
		})

	if parsedGrades == 0 {
		logrus.Debugf("No grades found in the scraped content for user %v", username)
	}

	return nil
}

// cleanEventDescription trims the exam event description, returning only the right side of the colon if it exists.
func cleanEventDescription(summary string) string {
	if idx := strings.Index(summary, ":"); idx != -1 {
		return strings.TrimSpace(summary[idx+1:])
	}

	return summary
}

// parseEvents processes Events array, emitting a single exam message for each event, optionally returning an
// error.
func parseEvents(msgPool *sync.Pool, ch chan<- *msgtypes.Message, username string, events fetch.Events) error {
	if len(events) == 0 {
		logrus.Debugf("No scheduled exams for user %v", username)
	}

	for _, ev := range events {
		subject := cleanEventDescription(ev.Summary)
		description := cleanEventDescription(ev.Description)
		timestamp := ev.Start.Format(TimeFormat)

		m := msgPool.Get().(*msgtypes.Message) //nolint:forcetypeassert
		m.IsExam = true
		m.Username = username
		m.Subject = subject
		m.Descriptions = []string{
			EventSummary,
			DateDescription,
			EventDescription,
		}
		m.Fields = []string{
			subject,
			timestamp,
			description,
		}
		ch <- m
	}

	return nil
}
