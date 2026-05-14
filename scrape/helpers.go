// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"unicode"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
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

// eventDescriptions is a shared, immutable slice used for all exam event messages to avoid per-event allocations.
var eventDescriptions = []string{
	EventSummary,
	DateDescription,
	EventDescription,
}

var trimBuilderPool = sync.Pool{
	New: func() any { return new(strings.Builder) },
}

// trimMaxPooledBuilderCap caps the capacity of pooled builders so a single very
// large input does not permanently inflate every entry in the pool.
const trimMaxPooledBuilderCap = 64 * 1024

// trimPutBuilder returns b to the pool after Reset. Resetting on Put (not just
// on Get) keeps every pooled entry in a clean state so callers cannot observe
// leftover content from a prior borrower. Builders whose backing buffer has
// grown beyond trimMaxPooledBuilderCap are dropped so one outlier input cannot
// bloat the pool indefinitely.
func trimPutBuilder(b *strings.Builder) {
	if b.Cap() > trimMaxPooledBuilderCap {
		return
	}

	b.Reset()
	trimBuilderPool.Put(b)
}

var (
	selNewGradesTable             = cascadia.MustCompile("div.content > div.flex-table.new-grades-table")
	selRowHeaderCellSpan          = cascadia.MustCompile("div.row.header div.cell > span")
	selRowNotHeader               = cascadia.MustCompile("div.row:not(.header)")
	selCellSpan                   = cascadia.MustCompile("div.cell > span")
	selStudentListClasses         = cascadia.MustCompile("div.student-list > div.classes")
	selClassMenuVerticalClassInfo = cascadia.MustCompile("div.class-menu-vertical:not(div.past-schoolyear) > div.class-info")
	selClassSpanBold              = cascadia.MustCompile("div.class > span.bold")
	selClassSpanSchoolyear        = cascadia.MustCompile("div.class > span.class-schoolyear")
	selSchoolSpanSchoolName       = cascadia.MustCompile("div.school > div > span.school-name")
	selContentUlListLiA           = cascadia.MustCompile("div.content > ul.list > li > a")
	selCourseInfoSpan             = cascadia.MustCompile("div.course-info > span")
	selNationalExamTable          = cascadia.MustCompile("div.content > div.flex-table.national-exam-table")
	selRowHeaderNotFirstCellSpan  = cascadia.MustCompile("div.row.header:not(.first) div.cell > span")
	selReadingsTable              = cascadia.MustCompile("div.content > div.flex-table.readings-table")
	selFinalGradeRow              = cascadia.MustCompile("div.content > div.flex-table.s.grades-table > div.row.final-grade")
	selCellBoldFirstSpan          = cascadia.MustCompile("div.cell.bold.first > span")
	selCellNotBoldFirstSpan       = cascadia.MustCompile("div.cell:not(.bold.first) > span")
)

// parseGrades extracts grades per subject from raw string (grade scrape response body) and grade descriptions,
// constructs grade messages and sends them a message channel, optionally returning an error.
func parseGrades(ctx context.Context, ch chan<- msgtypes.Message, username string, rawGrades []byte, multiClass bool, className string) error {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(rawGrades))
	if err != nil {
		return err
	}

	var parsedGrades int

	var cancelled bool

	doc.FindMatcher(selNewGradesTable).
		Each(func(_ int, table *goquery.Selection) {
			if cancelled {
				return
			}

			subject, subjectOK := table.Attr("data-action-id")
			if !subjectOK {
				return
			}

			if multiClass {
				subject = subject + " / " + className
			}

			headerCells := table.FindMatcher(selRowHeaderCellSpan)
			descriptions := make([]string, 0, headerCells.Length())

			headerCells.Each(func(_ int, column *goquery.Selection) {
				txt := strings.TrimSpace(column.Text())
				descriptions = append(descriptions, txt)
			})

			table.FindMatcher(selRowNotHeader).
				Each(func(_ int, row *goquery.Selection) {
					if cancelled {
						return
					}

					spanCells := row.FindMatcher(selCellSpan)
					spans := make([]string, 0, spanCells.Length())

					spanCells.Each(func(_ int, column *goquery.Selection) {
						txt := strings.TrimSpace(column.Text())
						if len(txt) > 0 {
							txt = trimAllSpace(txt)
						}

						spans = append(spans, txt)
					})

					select {
					case ch <- msgtypes.Message{
						Code:         msgtypes.Grade,
						Username:     username,
						Subject:      subject,
						Descriptions: descriptions,
						Fields:       spans,
					}:
						parsedGrades++
					case <-ctx.Done():
						cancelled = true
					}
				})
		})

	if cancelled {
		return ctx.Err()
	}

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
func parseEvents(ctx context.Context, ch chan<- msgtypes.Message, username string, events fetch.Events, multiClass bool, className string) error {
	if len(events) == 0 {
		logger.Info().Msgf("No scheduled exams for user %v", username)
	}

	for _, ev := range events {
		subject := cleanEventDescription(ev.Summary)
		description := cleanEventDescription(ev.Description)
		timestamp := ev.Start.Format(TimeFormat)

		if multiClass {
			subject = subject + " / " + className
		}

		select {
		case ch <- msgtypes.Message{
			Code:         msgtypes.Exam,
			Username:     username,
			Subject:      subject,
			Descriptions: eventDescriptions,
			Fields: []string{
				subject,
				timestamp,
				description,
			},
			Timestamp: ev.Start,
		}:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

// parseClasses extracts active classes from raw string (classes scrape response body) and constructs Classes structure
// with class ID, name, school name and year of enlistment.
func parseClasses(username string, rawClasses []byte) (fetch.Classes, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(rawClasses))
	if err != nil {
		return fetch.Classes{}, err
	}

	var parsedClasses int

	var classes fetch.Classes

	doc.FindMatcher(selStudentListClasses).
		Each(func(_ int, row *goquery.Selection) {
			row.FindMatcher(selClassMenuVerticalClassInfo).
				Each(func(_ int, column *goquery.Selection) {
					var c fetch.Class

					var idOK bool

					c.ID, idOK = column.Attr("data-action-id")
					if !idOK {
						return
					}

					if sel := column.FindMatcher(selClassSpanBold); sel.Length() > 0 {
						if sel.Length() > 1 {
							logger.Warn().Msgf("portal HTML drift: %d matches for class-name span on user %v (expected 1); using first", sel.Length(), username)
						}

						c.Name = strings.TrimSpace(sel.First().Text())
					}

					if sel := column.FindMatcher(selClassSpanSchoolyear); sel.Length() > 0 {
						if sel.Length() > 1 {
							logger.Warn().Msgf("portal HTML drift: %d matches for class-schoolyear span on user %v (expected 1); using first", sel.Length(), username)
						}

						c.Year = strings.TrimSpace(sel.First().Text())
					}

					if sel := column.FindMatcher(selSchoolSpanSchoolName); sel.Length() > 0 {
						if sel.Length() > 1 {
							logger.Warn().Msgf("portal HTML drift: %d matches for school-name span on user %v (expected 1); using first", sel.Length(), username)
						}

						c.School = strings.TrimSpace(sel.First().Text())
					}

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
func parseCourses(rawCourses []byte) (fetch.Courses, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(rawCourses))
	if err != nil {
		return fetch.Courses{}, err
	}

	var courses fetch.Courses

	doc.FindMatcher(selContentUlListLiA).
		Each(func(_ int, row *goquery.Selection) {
			href, hrefOK := row.Attr("href")
			if !hrefOK {
				return
			}

			span := row.FindMatcher(selCourseInfoSpan)
			if span.Length() > 0 {
				if span.Length() > 2 {
					logger.Warn().Msgf("portal HTML drift: %d matches for course-info span (expected 2); using first", span.Length())
				}

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
func parseCourse(ctx context.Context, ch chan<- msgtypes.Message, username string, rawCourse []byte, multiClass bool, className, subject string) error {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(rawCourse))
	if err != nil {
		return err
	}

	if multiClass {
		subject = subject + " / " + className
	}

	var cancelled bool

	doc.FindMatcher(selNationalExamTable).
		Each(func(_ int, table *goquery.Selection) {
			if cancelled {
				return
			}

			headerCells := table.FindMatcher(selRowHeaderNotFirstCellSpan)
			descriptions := make([]string, 0, headerCells.Length())

			headerCells.Each(func(_ int, column *goquery.Selection) {
				txt := strings.TrimSpace(column.Text())
				descriptions = append(descriptions, txt)
			})

			table.FindMatcher(selRowNotHeader).
				Each(func(_ int, row *goquery.Selection) {
					if cancelled {
						return
					}

					spanCells := row.FindMatcher(selCellSpan)
					spans := make([]string, 0, spanCells.Length())

					spanCells.Each(func(_ int, column *goquery.Selection) {
						txt := strings.TrimSpace(column.Text())
						if len(txt) > 0 {
							txt = trimAllSpace(txt)
						}

						spans = append(spans, txt)
					})

					if len(spans) > 0 && len(descriptions) > 0 {
						select {
						case ch <- msgtypes.Message{
							Code:         msgtypes.NationalExam,
							Username:     username,
							Subject:      subject,
							Fields:       spans,
							Descriptions: descriptions,
						}:
						case <-ctx.Done():
							cancelled = true
						}
					}
				})
		})

	doc.FindMatcher(selReadingsTable).
		Each(func(_ int, table *goquery.Selection) {
			if cancelled {
				return
			}

			headerCells := table.FindMatcher(selRowHeaderNotFirstCellSpan)
			descriptions := make([]string, 0, headerCells.Length())

			headerCells.Each(func(_ int, column *goquery.Selection) {
				txt := strings.TrimSpace(column.Text())
				descriptions = append(descriptions, txt)
			})

			table.FindMatcher(selRowNotHeader).
				Each(func(_ int, row *goquery.Selection) {
					if cancelled {
						return
					}

					spanCells := row.FindMatcher(selCellSpan)
					spans := make([]string, 0, spanCells.Length())

					spanCells.Each(func(_ int, column *goquery.Selection) {
						txt := strings.TrimSpace(column.Text())
						if len(txt) > 0 {
							txt = trimAllSpace(txt)
						}

						spans = append(spans, txt)
					})

					if len(spans) > 0 && len(descriptions) > 0 {
						select {
						case ch <- msgtypes.Message{
							Code:         msgtypes.Reading,
							Username:     username,
							Subject:      subject,
							Fields:       spans,
							Descriptions: descriptions,
						}:
						case <-ctx.Done():
							cancelled = true
						}
					}
				})
		})

	if cancelled {
		return ctx.Err()
	}

	// FinalGrade Subject must include className so hashes differ per school year.
	if !multiClass {
		subject = subject + " / " + className
	}

	doc.FindMatcher(selFinalGradeRow).
		Each(func(_ int, row *goquery.Selection) {
			if cancelled {
				return
			}

			descCells := row.FindMatcher(selCellBoldFirstSpan)
			descriptions := make([]string, 0, descCells.Length())

			descCells.Each(func(_ int, column *goquery.Selection) {
				txt := strings.TrimSpace(column.Text())
				descriptions = append(descriptions, txt)
			})

			spanCells := row.FindMatcher(selCellNotBoldFirstSpan)
			spans := make([]string, 0, spanCells.Length())

			spanCells.Each(func(_ int, column *goquery.Selection) {
				txt := strings.TrimSpace(column.Text())

				// Skip divisor cells.
				if len(txt) > 0 {
					spans = append(spans, txt)
				}
			})

			if len(spans) > 0 && len(descriptions) > 0 {
				select {
				case ch <- msgtypes.Message{
					Code:         msgtypes.FinalGrade,
					Username:     username,
					Subject:      subject,
					Fields:       spans,
					Descriptions: descriptions,
				}:
				case <-ctx.Done():
					cancelled = true
				}
			}
		})

	if cancelled {
		return ctx.Err()
	}

	return nil
}

// trimAllSpace removes all leading, trailing, and repeated spaces from the input string.
// It returns a single-space separated string.
func trimAllSpace(s string) string {
	needsMod := false
	inSpace := false
	firstNonSpace := false

	for _, r := range s {
		if unicode.IsSpace(r) {
			if !firstNonSpace {
				needsMod = true

				break
			}

			if inSpace || r != ' ' {
				needsMod = true

				break
			}

			inSpace = true
		} else {
			firstNonSpace = true
			inSpace = false
		}
	}

	if !needsMod && inSpace {
		needsMod = true
	}

	if !needsMod {
		return s
	}

	b := trimBuilderPool.Get().(*strings.Builder)
	defer trimPutBuilder(b)

	// No Reset needed: trimPutBuilder Resets before Put, so every Get yields a clean builder.
	b.Grow(len(s))

	inSpace = false

	for _, r := range s {
		if unicode.IsSpace(r) {
			if b.Len() > 0 {
				inSpace = true
			}
		} else {
			if inSpace {
				b.WriteByte(' ')

				inSpace = false
			}

			b.WriteRune(r)
		}
	}

	return b.String()
}
