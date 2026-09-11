// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/rs/zerolog"
)

// gradesTableWithNotes mirrors the live /grade/all markup after the portal
// added the "Bilješka" column (observed 2026-09-09): trailing cells moved into
// a div.box wrapper, the note cell holds a <pre> rather than a <span>, and the
// title row carries a span-less cell.
const gradesTableWithNotes = `<div class="content">
	<div class="flex-table new-grades-table" aria-label="NewGradesTable" data-action-id="Matematika">
		<div class="row header first">
			<div class="cell">Matematika</div>
		</div>
		<div class="row header">
			<div class="cell"><span>Datum</span></div>
			<div class="box">
				<div class="cell  "><span>Bilješka</span></div>
				<div class="cell"><span>Element vrednovanja</span></div>
				<div class="cell"><span>Ocjena</span></div>
			</div>
		</div>
		<div class="row ">
			<div class="cell"><span>8.9.</span></div>
			<div class="box">
				<div class="cell "><pre class="note-content">08.09.2026. Inicijalni ispit znanja</pre></div>
				<div class="cell"><span>stvaralaštvo</span></div>
				<div class="cell"> <span>5</span></div>
			</div>
		</div>
	</div>
</div>`

// TestParseGradesReadsNoteColumn pins each value under its own header. Dropping
// the span-less note cell mislabelled the whole row rather than failing
// visibly, which is why this asserts positions and not just presence.
func TestParseGradesReadsNoteColumn(t *testing.T) {
	t.Parallel()

	ch := make(chan msgtypes.Message, 4)

	if err := parseGrades(context.Background(), ch, "testuser", []byte(gradesTableWithNotes), false, "ClassA"); err != nil {
		t.Fatalf("parseGrades failed: %v", err)
	}

	close(ch)

	msgs := drain(ch)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}

	wantDesc := []string{"Datum", "Bilješka", "Element vrednovanja", "Ocjena"}
	if !reflect.DeepEqual(msgs[0].Descriptions, wantDesc) {
		t.Errorf("Descriptions = %q, want %q — the title row must not contribute a description",
			msgs[0].Descriptions, wantDesc)
	}

	wantFields := []string{"8.9.", "08.09.2026. Inicijalni ispit znanja", "stvaralaštvo", "5"}
	if !reflect.DeepEqual(msgs[0].Fields, wantFields) {
		t.Errorf("Fields = %q, want %q — one entry per cell keeps values under their own header",
			msgs[0].Fields, wantFields)
	}
}

// TestParseGradesNoteOnlyRowKeepsAlignment covers a note with no grade yet:
// the trailing cells must keep their positions so the note stays under
// "Bilješka" instead of the alert collapsing to a bare date.
func TestParseGradesNoteOnlyRowKeepsAlignment(t *testing.T) {
	t.Parallel()

	html := `<div class="content">
		<div class="flex-table new-grades-table" data-action-id="Matematika">
			<div class="row header first"><div class="cell">Matematika</div></div>
			<div class="row header">
				<div class="cell"><span>Datum</span></div>
				<div class="box">
					<div class="cell"><span>Bilješka</span></div>
					<div class="cell"><span>Element vrednovanja</span></div>
					<div class="cell"><span>Ocjena</span></div>
				</div>
			</div>
			<div class="row ">
				<div class="cell"><span>9.9.</span></div>
				<div class="box">
					<div class="cell"><pre class="note-content">Treba pisati postupak</pre></div>
					<div class="cell"></div>
					<div class="cell"></div>
				</div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 4)

	if err := parseGrades(context.Background(), ch, "testuser", []byte(html), false, "ClassA"); err != nil {
		t.Fatalf("parseGrades failed: %v", err)
	}

	close(ch)

	msgs := drain(ch)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}

	wantFields := []string{"9.9.", "Treba pisati postupak", "", ""}
	if !reflect.DeepEqual(msgs[0].Fields, wantFields) {
		t.Errorf("Fields = %q, want %q — the note must survive a row with no grade",
			msgs[0].Fields, wantFields)
	}
}

// TestParseGradesSkipsAllEmptyRow guards the padding's side effect: a row of
// empty cells is no longer an empty slice, so the contentless-row guard must
// test values rather than length or spacer rows become empty alerts.
func TestParseGradesSkipsAllEmptyRow(t *testing.T) {
	t.Parallel()

	html := `<div class="content">
		<div class="flex-table new-grades-table" data-action-id="Matematika">
			<div class="row header">
				<div class="cell"><span>Datum</span></div>
				<div class="cell"><span>Ocjena</span></div>
			</div>
			<div class="row "><div class="cell"></div><div class="cell"> </div></div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 4)

	if err := parseGrades(context.Background(), ch, "testuser", []byte(html), false, "ClassA"); err != nil {
		t.Fatalf("parseGrades failed: %v", err)
	}

	close(ch)

	if msgs := drain(ch); len(msgs) != 0 {
		t.Errorf("got %d messages, want 0 — a row with no values must not alert: %+v", len(msgs), msgs)
	}
}

// TestParseCourseFindsTablesUnderTabContent pins the div.tab-content wrapper
// the portal added to course pages. A direct-child combinator finds nothing
// through it, and a missing table is indistinguishable from an empty page, so
// final grades went missing with no error.
func TestParseCourseFindsTablesUnderTabContent(t *testing.T) {
	t.Parallel()

	html := `<div class="content">
		<div class="tab-content active">
			<div class="legend-text">x</div>
			<div class="flex-table s  grades-table " data-schoolyear="2026" aria-label="GradesTable">
				<div class="row header first">
					<div class="cell">Elementi vrednovanja:</div>
					<div class="cell">Ocjene po mjesecima</div>
				</div>
				<div class="row final-grade ">
					<div class="cell bold first"><span>ZAKLJUČENO</span></div>
					<div class="cell"></div>
					<div class="cell"><span>5</span></div>
				</div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 4)

	if err := parseCourse(context.Background(), ch, "testuser", []byte(html), false, "ClassA", "Matematika"); err != nil {
		t.Fatalf("parseCourse failed: %v", err)
	}

	close(ch)

	msgs := drain(ch)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1 FinalGrade — the tab-content wrapper must not hide the table", len(msgs))
	}

	if msgs[0].Code != msgtypes.FinalGrade {
		t.Errorf("Code = %v, want FinalGrade", msgs[0].Code)
	}

	if !reflect.DeepEqual(msgs[0].Fields, []string{"5"}) {
		t.Errorf("Fields = %q, want [\"5\"]", msgs[0].Fields)
	}
}

// captureLogs swaps the global logger for the duration of fn. It mutates global
// state, hence no t.Parallel() in its callers.
func captureLogs(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer

	saved := logger.Logger
	savedLevel := zerolog.GlobalLevel()

	logger.Logger = zerolog.New(&buf)
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	t.Cleanup(func() {
		logger.Logger = saved
		zerolog.SetGlobalLevel(savedLevel)
	})

	fn()

	return buf.String()
}

// TestEmptyResultDistinguishesDriftFromNoRecords covers the portal's own empty
// state (div.content.no-records) versus selectors that stopped matching. Both
// yield zero rows; only the second warrants a warning.
func TestEmptyResultDistinguishesDriftFromNoRecords(t *testing.T) {
	const (
		noRecords = `<html><body><div class="content no-records">` +
			`<div class="section-text no-title">Učenik nema upisanih ocjena.</div></div></body></html>`
		driftedAway = `<html><body><div class="content"><div class="flex-table renamed-table">` +
			`<div class="row"><div class="cell">4</div></div></div></div></body></html>`
	)

	tests := []struct {
		name     string
		html     string
		wantWarn bool
	}{
		{name: "grades, portal reports no records", html: noRecords, wantWarn: false},
		{name: "grades, selectors no longer match", html: driftedAway, wantWarn: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch := make(chan msgtypes.Message, 1)

			out := captureLogs(t, func() {
				if err := parseGrades(t.Context(), ch, "user@skole.hr", []byte(tt.html), false, "1.a"); err != nil {
					t.Fatalf("parseGrades() = %v, want nil", err)
				}
			})

			if got := strings.Contains(out, `"level":"warn"`); got != tt.wantWarn {
				t.Errorf("warning logged = %v, want %v\nlog output: %s", got, tt.wantWarn, out)
			}

			if tt.wantWarn && !strings.Contains(out, "drift") {
				t.Errorf("drift warning does not name drift, so an operator cannot act on it\nlog output: %s", out)
			}
		})
	}
}

// TestParseCoursesWarnsOnUnexplainedEmpty pins the same distinction for the
// course list, which gates national exams, readings and final grades at once.
func TestParseCoursesWarnsOnUnexplainedEmpty(t *testing.T) {
	const empty = `<html><body><div class="content"><ul class="list"></ul></div></body></html>`

	out := captureLogs(t, func() {
		courses, err := parseCourses("user@skole.hr", []byte(empty))
		if err != nil {
			t.Fatalf("parseCourses() = %v, want nil", err)
		}

		if len(courses) != 0 {
			t.Fatalf("parseCourses() = %d courses, want 0", len(courses))
		}
	})

	if !strings.Contains(out, `"level":"warn"`) {
		t.Errorf("an empty course list with no no-records marker must warn\nlog output: %s", out)
	}
}

// TestParseGradesNormalisesHeaderWhitespace holds header labels to the same
// contract as values: both moved to whole-cell .Text(), but only cellValues
// gained trimAllSpace, so nested markup kept its newlines and indentation —
// verbatim into every message and into the dedup hash.
func TestParseGradesNormalisesHeaderWhitespace(t *testing.T) {
	t.Parallel()

	html := `<div class="content">
		<div class="flex-table new-grades-table" data-action-id="Matematika">
			<div class="row header first"><div class="cell">Matematika</div></div>
			<div class="row header">
				<div class="cell"><span>Datum</span></div>
				<div class="box">
					<div class="cell">
						<i class="icon"></i>
						<span>Element</span>
						<span>vrednovanja</span>
					</div>
					<div class="cell"><span>Ocjena</span></div>
				</div>
			</div>
			<div class="row ">
				<div class="cell"><span>8.9.</span></div>
				<div class="box">
					<div class="cell"><span>stvaralaštvo</span></div>
					<div class="cell"><span>5</span></div>
				</div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 4)

	if err := parseGrades(context.Background(), ch, "testuser", []byte(html), false, "ClassA"); err != nil {
		t.Fatalf("parseGrades failed: %v", err)
	}

	close(ch)

	msgs := drain(ch)
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}

	want := []string{"Datum", "Element vrednovanja", "Ocjena"}
	if !reflect.DeepEqual(msgs[0].Descriptions, want) {
		t.Errorf("Descriptions = %q, want %q — interior whitespace must collapse as it does for values",
			msgs[0].Descriptions, want)
	}
}

// TestParseCourseNormalisesHeaderWhitespace extends the header/value contract to
// the national-exam and readings tables, whose values already go through
// trimAllSpace — a wrapped label carries interior whitespace inside one span.
func TestParseCourseNormalisesHeaderWhitespace(t *testing.T) {
	t.Parallel()

	html := `<div class="content">
		<div class="flex-table national-exam-table">
			<div class="row header first"><div class="cell"><span>Ispiti</span></div></div>
			<div class="row header">
				<div class="cell"><span>Predmet</span></div>
				<div class="cell"><span>Datum
					ispita</span></div>
			</div>
			<div class="row"><div class="cell"><span>Matematika A</span></div><div class="cell"><span>12.6.</span></div></div>
		</div>
		<div class="flex-table readings-table">
			<div class="row header first"><div class="cell"><span>Lektira</span></div></div>
			<div class="row header">
				<div class="cell"><span>Naslov</span></div>
				<div class="cell"><span>Datum
					unosa</span></div>
			</div>
			<div class="row"><div class="cell"><span>Ivan Gundulić</span></div><div class="cell"><span>3.3.</span></div></div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 8)

	if err := parseCourse(context.Background(), ch, "testuser", []byte(html), false, "ClassA", "Hrvatski"); err != nil {
		t.Fatalf("parseCourse failed: %v", err)
	}

	close(ch)

	msgs := drain(ch)
	if len(msgs) == 0 {
		t.Fatal("parseCourse produced no messages")
	}

	for _, m := range msgs {
		for _, d := range m.Descriptions {
			if strings.ContainsAny(d, "\n\t") || strings.Contains(d, "  ") {
				t.Errorf("description %q carries interior whitespace; values are normalised, headers must match", d)
			}
		}
	}
}

// TestParseCourseKeepsColumnAlignment extends cellValues' contract to the two
// remaining column-aligned tables. Reading values through `div.cell > span`
// drops any cell with no direct span — an empty one, or a <pre> note — sliding
// every later value a column left.
func TestParseCourseKeepsColumnAlignment(t *testing.T) {
	t.Parallel()

	html := `<div class="content">
		<div class="flex-table national-exam-table">
			<div class="row header first"><div class="cell"><span>Ispiti</span></div></div>
			<div class="row header">
				<div class="cell"><span>Naslov</span></div>
				<div class="cell"><span>Napomena</span></div>
				<div class="cell"><span>Ocjena</span></div>
			</div>
			<div class="row">
				<div class="cell"><span>Matematika A</span></div>
				<div class="cell"></div>
				<div class="cell"><span>4</span></div>
			</div>
		</div>
		<div class="flex-table readings-table">
			<div class="row header first"><div class="cell"><span>Lektira</span></div></div>
			<div class="row header">
				<div class="cell"><span>Naslov</span></div>
				<div class="cell"><span>Bilješka</span></div>
				<div class="cell"><span>Ocjena</span></div>
			</div>
			<div class="row">
				<div class="cell"><span>Proljeća Ivana Galeba</span></div>
				<div class="cell"><pre class="note-content">Treba pročitati do petka</pre></div>
				<div class="cell"><span>5</span></div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 8)

	if err := parseCourse(context.Background(), ch, "testuser", []byte(html), false, "ClassA", "Hrvatski"); err != nil {
		t.Fatalf("parseCourse failed: %v", err)
	}

	close(ch)

	byCode := map[msgtypes.EventCode]msgtypes.Message{}
	for _, m := range drain(ch) {
		byCode[m.Code] = m
	}

	exam, ok := byCode[msgtypes.NationalExam]
	if !ok {
		t.Fatal("no NationalExam message")
	}

	wantExam := []string{"Matematika A", "", "4"}
	if !reflect.DeepEqual(exam.Fields, wantExam) {
		t.Errorf("exam Fields = %q, want %q — the empty cell must hold its column", exam.Fields, wantExam)
	}

	reading, ok := byCode[msgtypes.Reading]
	if !ok {
		t.Fatal("no Reading message")
	}

	wantReading := []string{"Proljeća Ivana Galeba", "Treba pročitati do petka", "5"}
	if !reflect.DeepEqual(reading.Fields, wantReading) {
		t.Errorf("reading Fields = %q, want %q — a <pre> note is a value, not a missing cell",
			reading.Fields, wantReading)
	}
}

// TestParseCourseIgnoresInactiveSchoolYearTab scopes tables to the year on
// screen: the descendant combinator needed to reach through div.tab-content
// also reaches inactive tabs, alerting a past closing grade under the current
// class's name.
func TestParseCourseIgnoresInactiveSchoolYearTab(t *testing.T) {
	t.Parallel()

	html := `<div class="content">
		<div class="tab-content">
			<div class="flex-table s grades-table" data-schoolyear="2025">
				<div class="row header first"><div class="cell">Elementi</div></div>
				<div class="row final-grade "><div class="cell bold first"><span>ZAKLJUČENO</span></div><div class="cell"><span>2</span></div></div>
			</div>
			<div class="flex-table readings-table">
				<div class="row header first"><div class="cell"><span>Lektira</span></div></div>
				<div class="row header"><div class="cell"><span>Naslov</span></div></div>
				<div class="row"><div class="cell"><span>Stara lektira</span></div></div>
			</div>
		</div>
		<div class="tab-content active">
			<div class="flex-table s grades-table" data-schoolyear="2026">
				<div class="row header first"><div class="cell">Elementi</div></div>
				<div class="row final-grade "><div class="cell bold first"><span>ZAKLJUČENO</span></div><div class="cell"><span>5</span></div></div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 8)

	if err := parseCourse(context.Background(), ch, "testuser", []byte(html), false, "ClassA", "Matematika"); err != nil {
		t.Fatalf("parseCourse failed: %v", err)
	}

	close(ch)

	msgs := drain(ch)
	if len(msgs) != 1 {
		got := make([]string, 0, len(msgs))
		for _, m := range msgs {
			got = append(got, fmt.Sprintf("%v%q", m.Code, m.Fields))
		}

		t.Fatalf("got %d messages (%v), want only the active tab's final grade", len(msgs), got)
	}

	if !reflect.DeepEqual(msgs[0].Fields, []string{"5"}) {
		t.Errorf("Fields = %q, want [\"5\"] — the active school year's grade", msgs[0].Fields)
	}
}

// TestParseCourseKeepsTablesWhenNoTabIsActive fails open: if the active marker
// is renamed, scoping must not swallow every table. A scrape reporting nothing
// reads as a quiet school day.
func TestParseCourseKeepsTablesWhenNoTabIsActive(t *testing.T) {
	t.Parallel()

	html := `<div class="content">
		<div class="tab-content selected">
			<div class="flex-table s grades-table" data-schoolyear="2026">
				<div class="row header first"><div class="cell">Elementi</div></div>
				<div class="row final-grade "><div class="cell bold first"><span>ZAKLJUČENO</span></div><div class="cell"><span>5</span></div></div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 4)

	if err := parseCourse(context.Background(), ch, "testuser", []byte(html), false, "ClassA", "Matematika"); err != nil {
		t.Fatalf("parseCourse failed: %v", err)
	}

	close(ch)

	if msgs := drain(ch); len(msgs) != 1 {
		t.Errorf("got %d messages, want 1 — with no active tab anywhere, every table must still be read", len(msgs))
	}
}

// TestParseGradesIgnoresInactiveSchoolYearTab applies the same scoping to
// /grade/all. That page is not known to be tabbed — the wrapper was observed on
// course pages — but it uses the identical combinator, so it carries the
// identical hole; newTabScope is inert without tabs.
func TestParseGradesIgnoresInactiveSchoolYearTab(t *testing.T) {
	t.Parallel()

	html := `<div class="content">
		<div class="tab-content">
			<div class="flex-table new-grades-table" data-action-id="Matematika">
				<div class="row header first"><div class="cell">Matematika</div></div>
				<div class="row header"><div class="cell"><span>Datum</span></div></div>
				<div class="row "><div class="cell"><span>1.1.</span></div></div>
			</div>
		</div>
		<div class="tab-content active">
			<div class="flex-table new-grades-table" data-action-id="Matematika">
				<div class="row header first"><div class="cell">Matematika</div></div>
				<div class="row header"><div class="cell"><span>Datum</span></div></div>
				<div class="row "><div class="cell"><span>9.9.</span></div></div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 8)

	if err := parseGrades(context.Background(), ch, "testuser", []byte(html), false, "ClassA"); err != nil {
		t.Fatalf("parseGrades failed: %v", err)
	}

	close(ch)

	msgs := drain(ch)
	if len(msgs) != 1 {
		got := make([]string, 0, len(msgs))
		for _, m := range msgs {
			got = append(got, fmt.Sprintf("%q", m.Fields))
		}

		t.Fatalf("got %d messages (%v), want only the active tab's grade", len(msgs), got)
	}

	if !reflect.DeepEqual(msgs[0].Fields, []string{"9.9."}) {
		t.Errorf("Fields = %q, want [\"9.9.\"]", msgs[0].Fields)
	}
}
