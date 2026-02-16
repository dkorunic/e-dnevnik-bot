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

	"github.com/PuerkitoBio/goquery"
	"github.com/dkorunic/e-dnevnik-bot/fetch"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

const (
	TimeFormat       = "02.01.2006."  // DD.MM.YYYY. format
	DateDescription  = "Datum ispita" // exam date field description
	EventSummary     = "Predmet"      // exam summary field description (typically a subject name)
	EventDescription = "Napomena"     // exam remark field description (typically a target of the exam)
)

// parseGrades extracts grades per subject from raw string (grade scrape response body) and grade descriptions,
// constructs grade messages and sends them a message channel, optionally returning an error.
func parseGrades(ch chan<- msgtypes.Message, username, rawGrades string, multiClass bool, className string) error {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawGrades))
	if err != nil {
		return err
	}

	var parsedGrades int

	// each subject has a div with class "flex-table new-grades-table"
	doc.Find("div.content > div.flex-table.new-grades-table").
		Each(func(_ int, table *goquery.Selection) {
			// subject name is in data-action-id attribute
			subject, subjectOK := table.Attr("data-action-id")
			if !subjectOK {
				return
			}

			// if multiclass, append class name to subject
			if multiClass {
				subject = strings.Join([]string{subject, className}, " / ")
			}

			var descriptions []string

			// row descriptions are in div with class "row header" in each div with class "cell" in a span
			table.Find("div.row.header div.cell > span").
				Each(func(_ int, column *goquery.Selection) {
					txt := strings.TrimSpace(column.Text())
					descriptions = append(descriptions, txt)
				})

			// grades are in each div with class "row" (header rows excluded) ...
			table.Find("div.row:not(.header)").
				Each(func(_ int, row *goquery.Selection) {
					var spans []string

					// ... and in each div with class "cell" in a span
					row.Find("div.cell > span").
						Each(func(_ int, column *goquery.Selection) {
							// clean excess whitespace and newlines
							txt := strings.TrimSpace(column.Text())
							if len(txt) > 0 {
								txt = trimAllSpace(txt)
							}

							spans = append(spans, txt)
						})

					// once we have a single grade with all required fields, send it through the channel
					ch <- msgtypes.Message{
						Code:         msgtypes.Grade,
						Username:     username,
						Subject:      subject,
						Descriptions: descriptions,
						Fields:       spans,
					}

					parsedGrades++
				})
		})

	if parsedGrades == 0 {
		logger.Info().Msgf("No grades found in the scraped content for user %v", username)
	}

	return nil
}

// cleanEventDescription trims the exam event description, returning only the right side of the colon if it exists.
func cleanEventDescription(summary string) string {
	if _, after, ok := strings.Cut(summary, ":"); ok {
		return strings.TrimSpace(after)
	}

	return summary
}

// parseEvents processes Events array, emitting a single exam message for each event, optionally returning an
// error.
//
//nolint:unparam
func parseEvents(ch chan<- msgtypes.Message, username string, events fetch.Events, multiClass bool, className string) error {
	if len(events) == 0 {
		logger.Info().Msgf("No scheduled exams for user %v", username)
	}

	for _, ev := range events {
		subject := cleanEventDescription(ev.Summary)
		description := cleanEventDescription(ev.Description)
		timestamp := ev.Start.Format(TimeFormat)

		// if multiclass, append class name to subject
		if multiClass {
			subject = strings.Join([]string{subject, className}, " / ")
		}

		// send each event through channel
		ch <- msgtypes.Message{
			Code:     msgtypes.Exam,
			Username: username,
			Subject:  subject,
			Descriptions: []string{
				EventSummary,
				DateDescription,
				EventDescription,
			},
			Fields: []string{
				subject,
				timestamp,
				description,
			},
			Timestamp: ev.Start,
		}
	}

	return nil
}

// parseClasses extracts active classes from raw string (classes scrape response body) and constructs Classes structure
// with class ID, name, school name and year of enlistment.
func parseClasses(username, rawClasses string) (fetch.Classes, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawClasses))
	if err != nil {
		return fetch.Classes{}, err
	}

	var parsedClasses int

	var classes fetch.Classes

	// fetch all active classes
	doc.Find("div.student-list > div.classes").
		Each(func(_ int, row *goquery.Selection) {
			// div active classes are class-menu-vertical and not past-schoolyear
			row.Find("div.class-menu-vertical:not(div.past-schoolyear) > div.class-info").
				Each(func(_ int, column *goquery.Selection) {
					var c fetch.Class

					var idOK bool

					// class ID
					c.ID, idOK = column.Attr("data-action-id")
					if !idOK {
						return
					}

					// class name
					column.Find("div.class > span.bold").
						Each(func(_ int, span *goquery.Selection) {
							c.Name = strings.TrimSpace(span.Text())
						})

					// class year
					column.Find("div.class > span.class-schoolyear").
						Each(func(_ int, span *goquery.Selection) {
							c.Year = strings.TrimSpace(span.Text())
						})

					// class school
					column.Find("div.school > div > span.school-name").
						Each(func(_ int, span *goquery.Selection) {
							c.School = strings.TrimSpace(span.Text())
						})

					classes = append(classes, c)
					parsedClasses++
				})
		})

	if parsedClasses == 0 {
		logger.Info().Msgf("No active classes found in the scraped content for user %v", username)
	}

	return classes, nil
}

// parseCourses takes a raw HTML string of all courses a user is enrolled in, extracts
// the course name, teacher name, and URL, and returns a slice of fetch.Course objects.
func parseCourses(rawCourses string) (fetch.Courses, error) {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawCourses))
	if err != nil {
		return fetch.Courses{}, err
	}

	var courses fetch.Courses

	// list containing URLs for each course (but don't contain https schema or domain)
	doc.Find("div.content > ul.list > li > a").
		Each(func(_ int, row *goquery.Selection) {
			href, hrefOK := row.Attr("href")
			if !hrefOK {
				return
			}

			// and a course name
			span := row.Find("div.course-info > span")
			if span.Length() > 0 {
				courseName := strings.TrimSpace(span.First().Text())

				courses = append(courses, fetch.Course{
					Name: courseName,
					URL:  href,
				})
			}
		})

	return courses, nil
}

// parseCourse extracts course information (national exams, readings, final grades) from raw string
// and sends messages to a message channel, optionally returning an error.
func parseCourse(ch chan<- msgtypes.Message, username, rawCourse string, multiClass bool, className, subject string) error {
	doc, err := goquery.NewDocumentFromReader(strings.NewReader(rawCourse))
	if err != nil {
		return err
	}

	// if multiclass, append class name to subject
	if multiClass {
		subject = strings.Join([]string{subject, className}, " / ")
	}

	// process national-exam-table
	doc.Find("div.content > div.flex-table.national-exam-table").
		Each(func(_ int, table *goquery.Selection) {
			var descriptions []string

			// row descriptions are in div with class "row header" in each div with class "cell" in a span
			// skip over block header
			table.Find("div.row.header:not(.first) div.cell > span").
				Each(func(_ int, column *goquery.Selection) {
					txt := strings.TrimSpace(column.Text())
					descriptions = append(descriptions, txt)
				})

			table.Find("div.row:not(.header)").
				Each(func(_ int, row *goquery.Selection) {
					var spans []string

					// ... and in each div with class "cell" in a span
					row.Find("div.cell > span").
						Each(func(_ int, column *goquery.Selection) {
							// clean excess whitespace and newlines
							txt := strings.TrimSpace(column.Text())
							if len(txt) > 0 {
								txt = trimAllSpace(txt)
							}

							spans = append(spans, txt)
						})

					// we have a national-exam table entry, send it through the channel
					if len(spans) > 0 && len(descriptions) > 0 {
						ch <- msgtypes.Message{
							Code:         msgtypes.NationalExam,
							Username:     username,
							Subject:      subject,
							Fields:       spans,
							Descriptions: descriptions,
						}
					}
				})
		})

	// process readings-table
	doc.Find("div.content > div.flex-table.readings-table").
		Each(func(_ int, table *goquery.Selection) {
			var descriptions []string

			// row descriptions are in div with class "row header" in each div with class "cell" in a span
			// skip over block header
			table.Find("div.row.header:not(.first) div.cell > span").
				Each(func(_ int, column *goquery.Selection) {
					txt := strings.TrimSpace(column.Text())
					descriptions = append(descriptions, txt)
				})

			table.Find("div.row:not(.header)").
				Each(func(_ int, row *goquery.Selection) {
					var spans []string

					// ... and in each div with class "cell" in a span
					row.Find("div.cell > span").
						Each(func(_ int, column *goquery.Selection) {
							// clean excess whitespace and newlines
							txt := strings.TrimSpace(column.Text())
							if len(txt) > 0 {
								txt = trimAllSpace(txt)
							}

							spans = append(spans, txt)
						})

					// we have a readings table entry, send it through the channel
					if len(spans) > 0 && len(descriptions) > 0 {
						ch <- msgtypes.Message{
							Code:         msgtypes.Reading,
							Username:     username,
							Subject:      subject,
							Fields:       spans,
							Descriptions: descriptions,
						}
					}
				})
		})

	// final grades need additional subject suffix to have unique content for hash (which in case of multicass=false
	// was not added yet)
	if !multiClass {
		subject = strings.Join([]string{subject, className}, " / ")
	}

	// process final grades
	doc.Find("div.content > div.flex-table.s.grades-table > div.row.final-grade").
		Each(func(_ int, row *goquery.Selection) {
			var descriptions []string

			// first cell is a description ("ZAKLJUČENO")
			row.Find("div.cell.bold.first > span").
				Each(func(_ int, column *goquery.Selection) {
					txt := strings.TrimSpace(column.Text())
					descriptions = append(descriptions, txt)
				})

			var spans []string

			// following cells are final grades
			row.Find("div.cell:not(.bold.first) > span").
				Each(func(_ int, column *goquery.Selection) {
					// clean excess whitespace and newlines
					txt := strings.TrimSpace(column.Text())

					// it can be empty (ie. divisor)
					if len(txt) > 0 {
						spans = append(spans, txt)
					}
				})

			// send only if we have a final grade
			if len(spans) > 0 && len(descriptions) > 0 {
				ch <- msgtypes.Message{
					Code:         msgtypes.FinalGrade,
					Username:     username,
					Subject:      subject,
					Fields:       spans,
					Descriptions: descriptions,
				}
			}
		})

	return nil
}

// trimAllSpace removes all leading, trailing, and repeated spaces from the input string.
// It returns a single-space separated string.
func trimAllSpace(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")

	return strings.Join(strings.Fields(s), " ")
}
