// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"context"
	"reflect"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
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
