// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// driftGrades mirrors live /grade/all (2026-11): one table per subject,
// "Datum" first, "Ocjena" last.
func driftGrades(header string, grades ...string) string {
	var rows strings.Builder
	for i, g := range grades {
		fmt.Fprintf(&rows, `<div class="row "><div class="cell"><span>%d.9.</span></div><div class="box">
			<div class="cell "></div><div class="cell"><span>usmeno</span></div><div class="cell"><span>%s</span></div></div></div>`, i+1, g)
	}

	return `<div class="content"><div class="flex-table new-grades-table" data-action-id="Matematika">
		<div class="row header first"><div class="cell">Matematika</div></div>
		<div class="row header">` + header + `</div>` + rows.String() + `</div></div>`
}

const driftHeader = `<div class="cell"><span>Datum</span></div><div class="box">
	<div class="cell"><span>Bilješka</span></div><div class="cell"><span>Element vrednovanja</span></div>
	<div class="cell"><span>Ocjena</span></div></div>`

// driftCourses mirrors the live course list, whose div.list-average-grade is
// the portal's own average with trailing zeros trimmed ("4,5", "5").
func driftCourses(avg string) string {
	return `<div class="content"><ul class="list"><li><a href="/course/1">
		<div class="course-info"><span>Matematika</span><span>Nastavnik</span></div>
		<div class="list-average-grade">` + avg + `</div></a></li></ul></div>`
}

// TestCheckDrift pins the cross-check against the portal's own average: equal
// within its rounding is clean, anything else names the subject, and a subject
// the portal averages but the scrape lost entirely is the strongest signal.
func TestCheckDrift(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, grades, courses string
		want                  []string
	}{
		{"match", driftGrades(driftHeader, "5", "4"), driftCourses("4,5"), nil},
		{"portal rounding", driftGrades(driftHeader, "5", "4", "4"), driftCourses("4,33"), nil},
		{"portal truncation", driftGrades(driftHeader, "5", "5", "4"), driftCourses("4,66"), nil},
		{"one grade lost", driftGrades(driftHeader, "5", "4"), driftCourses("4,33"), []string{"prosjek ne odgovara ocjenama: Matematika"}},
		{"single grade", driftGrades(driftHeader, "5"), driftCourses("5"), nil},
		{"no portal average", driftGrades(driftHeader, "5", "4"), driftCourses(""), nil},
		{"mismatch", driftGrades(driftHeader, "5", "4"), driftCourses("3,5"), []string{"prosjek ne odgovara ocjenama: Matematika"}},
		{"table lost", `<div class="content"></div>`, driftCourses("4,5"), []string{"prosjek ne odgovara ocjenama: Matematika"}},
		{"unparseable average", driftGrades(driftHeader, "5", "4"), driftCourses("n/a"), []string{"prosjek ne odgovara ocjenama: Matematika"}},
		{
			// Reported once, as a header problem, not again as an average.
			"date column moved",
			driftGrades(`<div class="cell"><span>Ocjena</span></div><div class="cell"><span>Datum</span></div>`, "5", "4"),
			driftCourses("4,5"),
			[]string{"zaglavlje tablice ocjena: Matematika"},
		},
	} {
		if got := checkDrift("u", []byte(tc.grades), []byte(tc.courses)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: checkDrift = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCheckDriftSkipsDuplicateNames: a subject listed twice (split across
// groups) cannot be paired with one table, so it is skipped, not flagged.
func TestCheckDriftSkipsDuplicateNames(t *testing.T) {
	t.Parallel()

	twice := strings.Replace(driftCourses("3"), "</ul>", `<li><a href="/course/2">
		<div class="course-info"><span>Matematika</span><span>Drugi</span></div>
		<div class="list-average-grade">5</div></a></li></ul>`, 1)

	if got := checkDrift("u", []byte(driftGrades(driftHeader, "5", "4")), []byte(twice)); len(got) != 0 {
		t.Errorf("checkDrift = %q, want nothing for a name listed twice", got)
	}
}
