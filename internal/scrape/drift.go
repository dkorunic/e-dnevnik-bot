// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"bytes"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
)

const (
	// DateColumn must lead every grade table: isStaleEvent reads Fields[0] as
	// the grade date.
	DateColumn = "Datum"

	// averageTolerance absorbs the portal's two decimals whether it rounds or
	// truncates them (4.666… shown as "4,66" is 0.0067 off). A real drift is
	// far coarser: a lost or misread grade, or a whole column.
	averageTolerance = 0.01
)

// selListAverage is the portal's own per-subject average on the course list.
var selListAverage = cascadia.MustCompile("div.list-average-grade")

// CheckDrift turns the cross-checks on; main sets it from --driftcheck. Off,
// they do not run at all, so an opted-out install logs nothing about them.
var CheckDrift bool

// DriftError reports a scrape that completed, but whose pages no longer parse
// the way the portal itself presents them. Not a failure: the events already
// went out. Findings name subjects only, never grades.
type DriftError struct {
	Findings []string
}

func (e *DriftError) Error() string {
	return "portal HTML drift: " + strings.Join(e.Findings, "; ")
}

// checkDrift cross-checks /grade/all against the course list. A selector that
// stops matching shows up as an empty scrape, which logEmptyResult catches;
// what it cannot see is a scrape that still finds rows but reads them wrong.
// Two checks cover that:
//   - every grade table must lead with DateColumn and carry GradeDescription,
//     or Fields are mislabelled and the relevance filter reads the wrong cell;
//   - the mean of the scraped grades must equal the portal's own average for
//     the subject, which an independent server-side computation produces. A
//     lost row, a moved column or a renamed subject all break it.
//
// The average check races a teacher adding a grade between the two fetches;
// the caller requires a finding to repeat on the next cycle before alerting.
func checkDrift(username string, rawGrades, rawCourses []byte) []string {
	grades, err := goquery.NewDocumentFromReader(bytes.NewReader(rawGrades))
	if err != nil {
		return nil
	}

	courses, err := goquery.NewDocumentFromReader(bytes.NewReader(rawCourses))
	if err != nil {
		return nil
	}

	var findings []string

	// Subject → mean of its numeric grades; absent when it has none.
	means := make(map[string]float64)
	badHeader := make(map[string]bool)

	// A name on two tables or two course entries (a subject split across
	// groups) cannot be paired reliably, so it is not compared at all.
	tables := make(map[string]int)
	listed := make(map[string]int)

	scope := newTabScope(grades)

	grades.FindMatcher(selNewGradesTable).Each(func(_ int, table *goquery.Selection) {
		subject, ok := table.Attr("data-action-id")
		if !ok || !scope.includes(table) {
			return
		}

		tables[subject]++

		desc := headerDescriptions(table)
		if len(desc) == 0 || desc[0] != DateColumn || !slices.Contains(desc, GradeDescription) {
			findings = append(findings, "zaglavlje tablice ocjena: "+subject)
			badHeader[subject] = true

			return
		}

		var rows [][]string

		table.FindMatcher(selRowNotHeader).Each(func(_ int, row *goquery.Selection) {
			if fields, hasValue := cellValues(row); hasValue {
				rows = append(rows, fields)
			}
		})

		if mean, n := gradeMean(desc, rows); n > 0 {
			means[subject] = mean
		}
	})

	var averages int

	links := courses.FindMatcher(selContentUlListLiA)

	links.Each(func(_ int, a *goquery.Selection) {
		listed[strings.TrimSpace(a.FindMatcher(selCourseInfoSpan).First().Text())]++
	})

	links.Each(func(_ int, a *goquery.Selection) {
		name := strings.TrimSpace(a.FindMatcher(selCourseInfoSpan).First().Text())
		shown := trimAllSpace(a.FindMatcher(selListAverage).Text())

		if name == "" || shown == "" || badHeader[name] || tables[name] > 1 || listed[name] > 1 {
			return
		}

		averages++

		portal, err := strconv.ParseFloat(strings.Replace(shown, ",", ".", 1), 64)
		mean, scraped := means[name]

		if err != nil || !scraped || math.Abs(portal-mean) > averageTolerance {
			findings = append(findings, "prosjek ne odgovara ocjenama: "+name)
		}
	})

	// The oracle itself drifted: nothing is wrong with the alerts, but the
	// check above has gone blind, so say so where an operator looks.
	if averages == 0 && len(means) > 0 {
		logger.Warn().Msgf("No subject averages found on the course list for user %v: the portal drift check cannot run", username)
	}

	return findings
}
