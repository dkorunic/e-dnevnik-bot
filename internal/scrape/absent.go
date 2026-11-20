// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/dkorunic/e-dnevnik-bot/internal/fetch"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// absenceTotalLabel is the legend entry that sums the others; no row carries
// its colour.
const absenceTotalLabel = "Ukupno"

// ErrPageUnreadable marks an /absent or /notes page the reader cannot trust.
// Nothing of it is emitted: an identity built from a fallback (an undated day, a
// colour for a status) would change back once the page reads again, and alert
// the whole school year twice. The caller skips that code for the cycle.
var ErrPageUnreadable = errors.New("page unreadable")

// Shared and immutable, like eventDescriptions.
var absenceDescriptions = []string{"Datum", "Sat", "Razlog", "Status"}

// /absent holds one div.absent-table per absent day, for the whole school year:
// the month arrows only toggle visibility client-side.
var (
	selAbsentTable  = cascadia.MustCompile("div.content div.flex-table.absent-table")
	selAbsentLegend = cascadia.MustCompile("div.content div.absent-legend-table div.cell")
	selIconCircle   = cascadia.MustCompile("i.icon-circle")
	selSpan         = cascadia.MustCompile("span")
	selNoteTitle    = cascadia.MustCompile("div.content > div.title")
	selBr           = cascadia.MustCompile("br")
)

// croatianMonths maps the nominative month names /absent uses.
var croatianMonths = map[string]time.Month{
	"siječanj": time.January, "veljača": time.February, "ožujak": time.March,
	"travanj": time.April, "svibanj": time.May, "lipanj": time.June,
	"srpanj": time.July, "kolovoz": time.August, "rujan": time.September,
	"listopad": time.October, "studeni": time.November, "prosinac": time.December,
}

// schoolYearStart reads the first year of a class's school year, which the
// portal writes as "26/27" (and older pages as "2024/2025" or "2026./2027.").
func schoolYearStart(year string) (int, bool) {
	first, _, _ := strings.Cut(year, "/")

	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(first), "."))
	if err != nil || n < 0 {
		return 0, false
	}

	if n < 100 {
		n += 2000
	}

	return n, true
}

// absenceDate dates a day/month pair within the school year starting in
// startYear: September onwards is that year, the rest the next. ok is false for
// input it cannot date.
//
// The year is part of the dedup identity. Grades lack it and pay for that with
// a known miss across school years; a new event code can have it from the start.
func absenceDate(day, month string, startYear int) (time.Time, bool) {
	m, monthOK := croatianMonths[strings.ToLower(strings.TrimSpace(month))]
	d, err := strconv.Atoi(strings.TrimSpace(day))

	if !monthOK || err != nil {
		return time.Time{}, false
	}

	year := startYear
	if m < time.September {
		year++
	}

	t := time.Date(year, m, d, 0, 0, 0, 0, time.UTC)

	return t, t.Day() == d
}

// iconColour returns the colour class of an icon-circle, e.g. "gold".
func iconColour(icon *goquery.Selection) string {
	for c := range strings.FieldsSeq(icon.AttrOr("class", "")) {
		if c != "icon-circle" {
			return c
		}
	}

	return ""
}

// absenceLegend maps each status colour to its label and the portal's own
// count of hours with it.
type absenceLegend map[string]struct {
	label string
	count int
}

func parseAbsenceLegend(doc *goquery.Document) absenceLegend {
	legend := make(absenceLegend)

	doc.FindMatcher(selAbsentLegend).Each(func(_ int, cell *goquery.Selection) {
		colour := iconColour(cell.FindMatcher(selIconCircle).First())
		spans := cell.FindMatcher(selSpan)

		if colour == "" || spans.Length() < 2 {
			return
		}

		count, _ := strconv.Atoi(strings.TrimSpace(spans.Eq(1).Text()))
		legend[colour] = struct {
			label string
			count int
		}{strings.TrimSpace(spans.First().Text()), count}
	})

	return legend
}

// rowColour maps a row's icon colour to its legend entry. Pending rows are
// drawn "black" while their legend entry is "dark" (live, 2026-11); a legend
// that names the row's own colour wins, and one without "dark" gets the colour
// as drawn, so a finding names what the row really carries.
func rowColour(legend absenceLegend, colour string) string {
	if _, ok := legend[colour]; ok || colour != "black" {
		return colour
	}

	if _, ok := legend["dark"]; ok {
		return "dark"
	}

	return colour
}

// parseAbsences emits one message per absent lesson hour. The status is part of
// the identity on purpose: a pending absence that becomes unexcused alerts again.
//
// All or nothing: a page without a legend, a school year it cannot read, an
// undated day or a row colour the legend lacks returns ErrPageUnreadable and
// emits nothing (see there).
//
// Otherwise it returns the mismatches between the legend's counts and the rows
// it read, which are already logged; the caller adds them to the drift findings.
// Both come from one response, so unlike the grade average they cannot race.
func parseAbsences(ctx context.Context, ch chan<- msgtypes.Message, username string, raw []byte,
	multiClass bool, className, schoolYear string,
) ([]string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}

	legend := parseAbsenceLegend(doc)
	if len(legend) == 0 {
		return nil, fmt.Errorf("%w: absences: no legend", ErrPageUnreadable)
	}

	startYear, ok := schoolYearStart(schoolYear)
	if !ok {
		return nil, fmt.Errorf("%w: absences: school year %q", ErrPageUnreadable, schoolYear)
	}

	scraped := make(map[string]int)

	var msgs []msgtypes.Message

	var unreadable error

	doc.FindMatcher(selAbsentTable).Each(func(_ int, table *goquery.Selection) {
		if unreadable != nil {
			return
		}

		day, month := table.AttrOr("data-action-day", ""), table.AttrOr("data-action-id", "")

		ts, ok := absenceDate(day, month, startYear)
		if !ok {
			unreadable = fmt.Errorf("%w: absences: date %q %q", ErrPageUnreadable, day, month)

			return
		}

		table.FindMatcher(selRowNotHeader).Each(func(_ int, row *goquery.Selection) {
			if unreadable != nil {
				return
			}

			// div.cell hour, div.box (div.cell subject, div.cell reason), div.cell status icon.
			cols := row.Children()
			if cols.Length() < 3 {
				return
			}

			box := cols.Eq(1).FindMatcher(selCell)
			subject := trimAllSpace(box.First().Text())

			if subject == "" {
				return
			}

			colour := rowColour(legend, iconColour(row.FindMatcher(selIconCircle).First()))

			l, ok := legend[colour]
			if !ok || l.label == absenceTotalLabel {
				unreadable = fmt.Errorf("%w: absences: row colour %q, which the legend lacks", ErrPageUnreadable, colour)

				return
			}

			scraped[colour]++

			if multiClass {
				subject += " / " + className
			}

			msgs = append(msgs, msgtypes.Message{
				Code:         msgtypes.Absence,
				Username:     username,
				Subject:      subject,
				Class:        className,
				Descriptions: absenceDescriptions,
				Fields: []string{
					ts.Format(TimeFormat),
					trimAllSpace(cols.Eq(0).Text()),
					trimAllSpace(box.Eq(1).Text()),
					l.label,
				},
				Timestamp: ts,
			})
		})
	})

	if unreadable != nil {
		return nil, unreadable
	}

	var mismatches []string

	// Sorted, so the findings and their truncation are stable across cycles.
	for _, colour := range slices.Sorted(maps.Keys(legend)) {
		l := legend[colour]
		if l.label == absenceTotalLabel || l.count == scraped[colour] {
			continue
		}

		f := fmt.Sprintf("absences %v: %d on the portal, %d scraped", l.label, l.count, scraped[colour])
		logger.Warn().Msgf("Absence count mismatch for user %v, %v: possible portal HTML drift", username, f)

		mismatches = append(mismatches, f)
	}

	for _, m := range msgs {
		select {
		case ch <- m:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return mismatches, nil
}

// pageSkips collects, over a user's classes, the codes whose optional page was
// skipped and, with CheckDrift, the drift findings those pages produced.
type pageSkips struct {
	codes []msgtypes.EventCode
	drift []string
}

// skip absorbs an optional page's failure, unless the scrape itself is over
// (shutdown, budget): the code is skipped for the cycle and, with CheckDrift, a
// page that keeps failing becomes a finding.
func (p *pageSkips) skip(ctx context.Context, username string, code msgtypes.EventCode, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}

	logger.Warn().Msgf("Skipping code %v for user %v this cycle: %v", code, username, err)

	if !slices.Contains(p.codes, code) {
		p.codes = append(p.codes, code)
	}

	if CheckDrift {
		p.drift = append(p.drift, err.Error())
	}

	return nil
}

// flushDrift hands over the findings gathered since the last call.
func (p *pageSkips) flushDrift() []string {
	d := p.drift
	p.drift = nil

	return d
}

// optionalPages fetches and parses one class's /absent and /notes. Both are
// optional: a failure, or a page the reader cannot trust, skips that code for
// the cycle instead of costing the course pages and later classes. It returns
// an error only once the scrape itself is over.
func optionalPages(ctx context.Context, ch chan<- msgtypes.Message, client *fetch.Client,
	withRetry func(func() error) error, username string, multiClass bool, c fetch.Class, skipped *pageSkips,
) error {
	var raw []byte

	err := withRetry(func() error {
		var err error
		raw, err = client.GetAbsences()

		return markPermanent(err)
	})

	var mismatches []string
	if err == nil {
		mismatches, err = parseAbsences(ctx, ch, username, raw, multiClass, c.Name, c.Year)
	}

	if err = skipped.skip(ctx, username, msgtypes.Absence, err); err != nil {
		return err
	}

	if CheckDrift {
		for _, f := range mismatches {
			if multiClass {
				f += " / " + c.Name
			}

			skipped.drift = append(skipped.drift, f)
		}
	}

	err = withRetry(func() error {
		var err error
		raw, err = client.GetNotes()

		return markPermanent(err)
	})
	if err == nil {
		err = parseNotes(ctx, ch, username, raw, c.Name)
	}

	return skipped.skip(ctx, username, msgtypes.Note, err)
}

// isNotePlaceholder reports the portal's "nothing recorded" texts, as captured
// live (2026-11): "Učenik nema pedagoških mjera.", "Bilješka razrednika nije
// upisana." and the like. The class is in a note's identity, so emitting them
// would alert every one of them each September.
func isNotePlaceholder(text string) bool {
	return !strings.Contains(text, "\n") &&
		(strings.HasPrefix(text, "Učenik nema ") || strings.HasSuffix(text, " nije upisana."))
}

// parseNotes emits one message per titled section of /notes (class teacher,
// teacher's note, activities, behaviour, measures), so editing any one alerts.
// Placeholder sections are left out, so a section alerts once it has content.
//
// The class always goes in the Subject, as for final grades: the same behaviour
// mark in next year's class must not hash like this year's.
//
// A page with no sections at all returns ErrPageUnreadable: the live page always
// carries them, placeholders included.
func parseNotes(ctx context.Context, ch chan<- msgtypes.Message, username string, raw []byte, className string) error {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}

	var sections int

	var msgs []msgtypes.Message

	doc.FindMatcher(selNoteTitle).Each(func(_ int, title *goquery.Selection) {
		section := title.Next()
		if !section.HasClass("section-text") {
			return
		}

		sections++

		name := trimAllSpace(title.Text())
		text := noteText(section)

		if name == "" || text == "" || isNotePlaceholder(text) {
			return
		}

		msgs = append(msgs, msgtypes.Message{
			Code:         msgtypes.Note,
			Username:     username,
			Subject:      name + " / " + className,
			Descriptions: []string{name},
			Fields:       []string{text},
		})
	})

	if sections == 0 {
		return fmt.Errorf("%w: notes: no sections", ErrPageUnreadable)
	}

	for _, m := range msgs {
		select {
		case ch <- m:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return nil
}

// noteText keeps the section's <br> line breaks, which carry its structure
// (teacher, school year, consultation hours), and collapses everything else.
func noteText(section *goquery.Selection) string {
	section = section.Clone()
	section.FindMatcher(selBr).ReplaceWithHtml("\n")

	var lines []string

	for line := range strings.SplitSeq(section.Text(), "\n") {
		if line = trimAllSpace(line); line != "" {
			lines = append(lines, line)
		}
	}

	return strings.Join(lines, "\n")
}
