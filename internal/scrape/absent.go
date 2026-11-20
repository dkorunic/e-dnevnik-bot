// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// absenceTotalLabel is the legend entry that sums the others; no row carries
// its colour.
const absenceTotalLabel = "Ukupno"

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
// startYear: September onwards is that year, the rest the next.
//
// The year is part of the dedup identity. Grades lack it and pay for that with
// a known miss across school years; a new event code can have it from the start.
// Unresolvable input falls back to the portal's own text, so the alert still
// goes out, only without a year.
func absenceDate(day, month string, startYear int, yearOK bool) (string, time.Time) {
	m, monthOK := croatianMonths[strings.ToLower(strings.TrimSpace(month))]
	d, err := strconv.Atoi(strings.TrimSpace(day))

	if !monthOK || err != nil || !yearOK {
		return strings.TrimSpace(day) + ". " + strings.TrimSpace(month), time.Time{}
	}

	year := startYear
	if m < time.September {
		year++
	}

	t := time.Date(year, m, d, 0, 0, 0, 0, time.UTC)
	if t.Day() != d {
		return strings.TrimSpace(day) + ". " + strings.TrimSpace(month), time.Time{}
	}

	return t.Format(TimeFormat), t
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

// parseAbsences emits one message per absent lesson hour. The status is part of
// the identity on purpose: a pending absence that becomes unexcused alerts again.
//
// It returns the mismatches between the legend's counts and the rows it read,
// which are already logged; the caller adds them to the drift findings. Both
// come from one response, so unlike the grade average they cannot race.
func parseAbsences(ctx context.Context, ch chan<- msgtypes.Message, username string, raw []byte,
	multiClass bool, className, schoolYear string,
) ([]string, error) {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}

	legend := parseAbsenceLegend(doc)
	startYear, yearOK := schoolYearStart(schoolYear)
	scraped := make(map[string]int)

	var cancelled bool

	doc.FindMatcher(selAbsentTable).Each(func(_ int, table *goquery.Selection) {
		if cancelled {
			return
		}

		date, ts := absenceDate(table.AttrOr("data-action-day", ""), table.AttrOr("data-action-id", ""),
			startYear, yearOK)
		if ts.IsZero() {
			logger.Warn().Msgf("Unable to date an absence for user %v from %q (school year %q): possible portal HTML drift",
				username, date, schoolYear)
		}

		table.FindMatcher(selRowNotHeader).Each(func(_ int, row *goquery.Selection) {
			if cancelled {
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

			colour := iconColour(row.FindMatcher(selIconCircle).First())

			// Pending rows are drawn "black", their legend entry "dark" (live,
			// 2026-11). A legend that names the row's own colour wins.
			if _, ok := legend[colour]; !ok && colour == "black" {
				colour = "dark"
			}

			scraped[colour]++

			status := legend[colour].label
			if status == "" {
				status = colour
			}

			if multiClass {
				subject += " / " + className
			}

			select {
			case ch <- msgtypes.Message{
				Code:         msgtypes.Absence,
				Username:     username,
				Subject:      subject,
				Class:        className,
				Descriptions: absenceDescriptions,
				Fields: []string{
					date,
					trimAllSpace(cols.Eq(0).Text()),
					trimAllSpace(box.Eq(1).Text()),
					status,
				},
				Timestamp: ts,
			}:
			case <-ctx.Done():
				cancelled = true
			}
		})
	})

	if cancelled {
		return nil, ctx.Err()
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

	// A row colour the legend lacks still alerts, with the colour as its status,
	// but is named here so the next alias is found from the log, not the alerts.
	for _, colour := range slices.Sorted(maps.Keys(scraped)) {
		if _, ok := legend[colour]; ok || len(legend) == 0 {
			continue
		}

		f := fmt.Sprintf("absences: %d rows with colour %q, which the legend lacks", scraped[colour], colour)
		logger.Warn().Msgf("Unknown absence status for user %v, %v: possible portal HTML drift", username, f)

		mismatches = append(mismatches, f)
	}

	if len(legend) == 0 {
		logger.Warn().Msgf("No absence legend parsed for user %v: possible portal HTML drift", username)
	}

	return mismatches, nil
}

// parseNotes emits one message per titled section of /notes (class teacher,
// teacher's note, activities, behaviour, measures), so editing any one alerts.
//
// The class always goes in the Subject, as for final grades: the same behaviour
// mark in next year's class must not hash like this year's.
func parseNotes(ctx context.Context, ch chan<- msgtypes.Message, username string, raw []byte, className string) error {
	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(raw))
	if err != nil {
		return err
	}

	var parsed int

	var cancelled bool

	doc.FindMatcher(selNoteTitle).Each(func(_ int, title *goquery.Selection) {
		section := title.Next()
		if cancelled || !section.HasClass("section-text") {
			return
		}

		name := trimAllSpace(title.Text())
		text := noteText(section)

		if name == "" || text == "" {
			return
		}

		select {
		case ch <- msgtypes.Message{
			Code:         msgtypes.Note,
			Username:     username,
			Subject:      name + " / " + className,
			Descriptions: []string{name},
			Fields:       []string{text},
		}:
			parsed++
		case <-ctx.Done():
			cancelled = true
		}
	})

	if cancelled {
		return ctx.Err()
	}

	if parsed == 0 {
		logEmptyResult(doc, username, "notes")
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
