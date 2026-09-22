// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"bytes"
	"context"
	"strings"
	"unicode"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/dkorunic/e-dnevnik-bot/internal/fetch"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

const (
	TimeFormat       = "02.01.2006."  // DD.MM.YYYY. format
	DateDescription  = "Datum ispita" // exam date field description
	EventSummary     = "Predmet"      // exam summary field description (typically a subject name)
	EventDescription = "Napomena"     // exam remark field description (typically a target of the exam)
)

// Shared and immutable, to avoid an allocation per exam event.
var eventDescriptions = []string{
	EventSummary,
	DateDescription,
	EventDescription,
}

// Course-page tables sit inside div.tab-content, so the descendant combinator is
// required: "div.content >" reaches none of them, and finds nothing rather than
// failing.
var (
	selNewGradesTable             = cascadia.MustCompile("div.content div.flex-table.new-grades-table")
	selRowHeaderNotFirstCell      = cascadia.MustCompile("div.row.header:not(.first) div.cell")
	selRowNotHeader               = cascadia.MustCompile("div.row:not(.header)")
	selCell                       = cascadia.MustCompile("div.cell")
	selStudentListClasses         = cascadia.MustCompile("div.student-list > div.classes")
	selClassMenuVerticalClassInfo = cascadia.MustCompile("div.class-menu-vertical:not(div.past-schoolyear) > div.class-info")
	selClassSpanBold              = cascadia.MustCompile("div.class > span.bold")
	selClassSpanSchoolyear        = cascadia.MustCompile("div.class > span.class-schoolyear")
	selSchoolSpanSchoolName       = cascadia.MustCompile("div.school > div > span.school-name")
	selContentUlListLiA           = cascadia.MustCompile("div.content > ul.list > li > a")
	selCourseInfoSpan             = cascadia.MustCompile("div.course-info > span")
	selNationalExamTable          = cascadia.MustCompile("div.content div.flex-table.national-exam-table")
	selReadingsTable              = cascadia.MustCompile("div.content div.flex-table.readings-table")
	selFinalGradeRow              = cascadia.MustCompile("div.content div.flex-table.s.grades-table > div.row.final-grade")
	selCellBoldFirstSpan          = cascadia.MustCompile("div.cell.bold.first > span")
	selCellNotBoldFirstSpan       = cascadia.MustCompile("div.cell:not(.bold.first) > span")

	// The portal's explicit empty state, e.g. "Učenik nema upisanih ocjena.".
	selNoRecords = cascadia.MustCompile("div.content.no-records")

	selTabContent       = cascadia.MustCompile("div.tab-content")
	selTabContentActive = cascadia.MustCompile("div.tab-content.active")
)

// tabScope keeps tables to the school year on screen. The descendant combinators
// above are needed to reach through div.tab-content at all, but they also match
// inactive years, where a past closing grade would alert under the current
// class's name.
//
// Fails open on both no tabs and no .active: a silent empty scrape reads as a
// quiet school day, which is worse than a duplicate.
type tabScope struct {
	enforce bool
}

func newTabScope(doc *goquery.Document) tabScope {
	return tabScope{enforce: doc.FindMatcher(selTabContentActive).Length() > 0}
}

func (s tabScope) includes(sel *goquery.Selection) bool {
	if !s.enforce {
		return true
	}

	tab := sel.ClosestMatcher(selTabContent)
	if tab.Length() == 0 {
		return true
	}

	return tab.HasClass("active")
}

// logEmptyResult separates the portal's own empty state from selectors that
// stopped matching. Both yield zero rows, and conflating them is how a drifted
// scrape stays invisible — it reads as a quiet school day.
func logEmptyResult(doc *goquery.Document, username, what string) {
	if doc.FindMatcher(selNoRecords).Length() > 0 {
		logger.Debug().Msgf("Portal reports no %v recorded for user %v", what, username)

		return
	}

	logger.Warn().Msgf("No %v parsed for user %v, and the portal did not flag an empty record set: possible portal HTML drift",
		what, username)
}

// parseGrades emits one message per grade row, per subject.
func parseGrades(ctx context.Context, ch chan<- msgtypes.Message, username string, rawGrades []byte, multiClass bool, className string) error {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(rawGrades))
	if err != nil {
		return err
	}

	var parsedGrades int

	scope := newTabScope(doc)

	var cancelled bool

	doc.FindMatcher(selNewGradesTable).
		Each(func(_ int, table *goquery.Selection) {
			if cancelled || !scope.includes(table) {
				return
			}

			subject, subjectOK := table.Attr("data-action-id")
			if !subjectOK {
				return
			}

			if multiClass {
				subject += " / " + className
			}

			descriptions := headerDescriptions(table)

			table.FindMatcher(selRowNotHeader).
				Each(func(_ int, row *goquery.Selection) {
					if cancelled {
						return
					}

					fields, hasValue := cellValues(row)

					// No fieldless alerts.
					if !hasValue || len(descriptions) == 0 {
						return
					}

					select {
					case ch <- msgtypes.Message{
						Code:         msgtypes.Grade,
						Username:     username,
						Subject:      subject,
						Descriptions: descriptions,
						Fields:       fields,
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
		logEmptyResult(doc, username, "grades")
	}

	return nil
}

// cleanEventDescription keeps only the text after a colon, when there is one.
func cleanEventDescription(summary string) string {
	if _, after, ok := strings.Cut(summary, ":"); ok {
		return strings.TrimSpace(after)
	}

	return summary
}

// parseEvents emits one exam message per calendar event.
func parseEvents(ctx context.Context, ch chan<- msgtypes.Message, username string, events fetch.Events, multiClass bool, className string) error {
	if len(events) == 0 {
		logger.Info().Msgf("No scheduled exams for user %v", username)
	}

	for _, ev := range events {
		subject := cleanEventDescription(ev.Summary)
		description := cleanEventDescription(ev.Description)
		timestamp := ev.Start.Format(TimeFormat)

		if multiClass {
			subject += " / " + className
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

// parseClasses extracts the active classes: ID, name, school and year.
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

	// Legitimate over the summer break, so not an error — but downstream it is
	// indistinguishable from a healthy poll, which makes this the only signal
	// that a login succeeded while alerts silently never fire.
	if parsedClasses == 0 {
		logger.Warn().Msgf("No active classes found in the scraped content for user %v", username)
	}

	return classes, nil
}

// parseCourses extracts the name and URL of every enrolled course.
func parseCourses(username string, rawCourses []byte) (fetch.Courses, error) {
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

	// Every course page is reached through this list, so an empty result silences
	// national exams, readings and final grades at once.
	if len(courses) == 0 {
		logEmptyResult(doc, username, "courses")
	}

	return courses, nil
}

// emitFlexTable emits one message per content-carrying row of every table
// matching sel, pairing each row's cells with that table's header labels.
//
// National exams and readings differ only in selector and event code, so they
// share this reader — the row handling once existed three times over and was
// fixed in only one (see cellValues). Add a new flex table as another call,
// never another copy.
//
// Reports cancellation so the caller can surface ctx.Err() rather than report a
// truncated scrape as complete.
func emitFlexTable(ctx context.Context, ch chan<- msgtypes.Message, doc *goquery.Document, scope tabScope,
	sel goquery.Matcher, code msgtypes.EventCode, username, subject string,
) bool {
	var cancelled bool

	doc.FindMatcher(sel).
		Each(func(_ int, table *goquery.Selection) {
			if cancelled || !scope.includes(table) {
				return
			}

			descriptions := headerDescriptions(table)

			table.FindMatcher(selRowNotHeader).
				Each(func(_ int, row *goquery.Selection) {
					if cancelled {
						return
					}

					fields, hasValue := cellValues(row)

					// No fieldless alerts.
					if !hasValue || len(descriptions) == 0 {
						return
					}

					select {
					case ch <- msgtypes.Message{
						Code:         code,
						Username:     username,
						Subject:      subject,
						Fields:       fields,
						Descriptions: descriptions,
					}:
					case <-ctx.Done():
						cancelled = true
					}
				})
		})

	return cancelled
}

// parseCourse emits a course page's national exams, readings and final grade.
func parseCourse(ctx context.Context, ch chan<- msgtypes.Message, username string, rawCourse []byte, multiClass bool, className, subject string) error {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(rawCourse))
	if err != nil {
		return err
	}

	if multiClass {
		subject += " / " + className
	}

	scope := newTabScope(doc)

	if emitFlexTable(ctx, ch, doc, scope, selNationalExamTable, msgtypes.NationalExam, username, subject) {
		return ctx.Err()
	}

	if emitFlexTable(ctx, ch, doc, scope, selReadingsTable, msgtypes.Reading, username, subject) {
		return ctx.Err()
	}

	// className must be in the Subject, or hashes collide across school years.
	if !multiClass {
		subject += " / " + className
	}

	// Not emitFlexTable: a single label/value row that deliberately compacts
	// empty cells, rather than a padded flex table.
	var cancelled bool

	doc.FindMatcher(selFinalGradeRow).
		Each(func(_ int, row *goquery.Selection) {
			if cancelled || !scope.includes(row) {
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

				// Divisor cell.
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

// headerDescriptions returns one entry per header cell, in column order — the
// labels cellValues' values pair against.
//
// Whole-cell .Text(), not a `> span` read, for the same reason cellValues uses
// it: the two must count the same cells or every value lands under the wrong
// header. Kept adjacent to cellValues so the pairing stays visible.
//
// :not(.first) drops the title row, whose lone subject-name cell would offset
// every column.
func headerDescriptions(table *goquery.Selection) []string {
	headerCells := table.FindMatcher(selRowHeaderNotFirstCell)
	descriptions := make([]string, 0, headerCells.Length())

	headerCells.Each(func(_ int, column *goquery.Selection) {
		txt := strings.TrimSpace(column.Text())
		if len(txt) > 0 {
			// Whole-cell .Text() carries whitespace between nested elements;
			// cellValues normalises the same way.
			txt = trimAllSpace(txt)
		}

		descriptions = append(descriptions, txt)
	})

	return descriptions
}

// cellValues returns one entry per div.cell in row, in column order, reporting
// whether any carried text.
//
// Cell text, not a child <span>: the "Bilješka" column holds a <pre>, which a
// span-only read omits, sliding every later value left of its header. Empty
// cells stay as padding so Fields[i] lines up with Descriptions[i] — hence the
// bool, since an all-empty row is still a non-empty slice.
func cellValues(row *goquery.Selection) ([]string, bool) {
	cells := row.FindMatcher(selCell)
	values := make([]string, 0, cells.Length())

	var hasValue bool

	cells.Each(func(_ int, cell *goquery.Selection) {
		txt := strings.TrimSpace(cell.Text())
		if len(txt) > 0 {
			// <pre> notes arrive with newlines.
			txt = trimAllSpace(txt)
			hasValue = true
		}

		values = append(values, txt)
	})

	return values, hasValue
}

// trimAllSpace collapses every run of whitespace to a single space and trims
// the ends.
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

	// Local, never pooled: String() aliases this buffer, and the result becomes
	// part of Message.Fields — the dedup identity.
	var b strings.Builder

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
