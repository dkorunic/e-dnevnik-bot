// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"bytes"
	"context"
	"testing"

	"github.com/PuerkitoBio/goquery"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// flexTableHTML renders a course page carrying one national-exam table and one
// readings table with identical structure, so a single fixture can prove both
// readers behave the same way.
const flexTableHTML = `<div class="content">
  <div class="flex-table national-exam-table">
    <div class="row header first"><div class="cell">Nacionalni ispiti</div></div>
    <div class="row header"><div class="cell">Datum</div><div class="cell">Ispit</div><div class="cell">Bilješka</div></div>
    <div class="row"><div class="cell">1.3.</div><div class="cell">Matematika</div><div class="cell"><pre>  dobar   rezultat
 </pre></div></div>
    <div class="row"><div class="cell"></div><div class="cell"></div><div class="cell"></div></div>
  </div>
  <div class="flex-table readings-table">
    <div class="row header first"><div class="cell">Lektire</div></div>
    <div class="row header"><div class="cell">Datum</div><div class="cell">Ispit</div><div class="cell">Bilješka</div></div>
    <div class="row"><div class="cell">1.3.</div><div class="cell">Matematika</div><div class="cell"><pre>  dobar   rezultat
 </pre></div></div>
    <div class="row"><div class="cell"></div><div class="cell"></div><div class="cell"></div></div>
  </div>
</div>`

// docFrom parses raw into a goquery document for the flex-table tests.
func docFrom(t *testing.T, raw string) *goquery.Document {
	t.Helper()

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader([]byte(raw)))
	if err != nil {
		t.Fatalf("goquery.NewDocumentFromReader() failed: %v", err)
	}

	return doc
}

// collect drains ch until it is closed, returning everything emitted.
func collect(ch <-chan msgtypes.Message) []msgtypes.Message {
	var got []msgtypes.Message

	for m := range ch {
		got = append(got, m)
	}

	return got
}

// TestEmitFlexTableTreatsBothTablesIdentically is the regression guard that
// motivated sharing one reader. National exams and readings previously had a
// copied reader each; a fix applied to one silently left the other behind —
// the same failure mode that once shifted every value one column left.
//
// Structurally identical input must therefore produce structurally identical
// output, differing only in the event code.
func TestEmitFlexTableTreatsBothTablesIdentically(t *testing.T) {
	t.Parallel()

	doc := docFrom(t, flexTableHTML)
	scope := newTabScope(doc)

	emit := func(sel goquery.Matcher, code msgtypes.EventCode) []msgtypes.Message {
		ch := make(chan msgtypes.Message, 8)
		cancelled := emitFlexTable(t.Context(), ch, doc, scope, sel, code, "u@skole.hr", "Matematika")
		close(ch)

		if cancelled {
			t.Fatalf("emitFlexTable() reported cancelled on a live context")
		}

		return collect(ch)
	}

	exams := emit(selNationalExamTable, msgtypes.NationalExam)
	readings := emit(selReadingsTable, msgtypes.Reading)

	if len(exams) != 1 || len(readings) != 1 {
		t.Fatalf("emitted %d national exam(s) and %d reading(s), want 1 each", len(exams), len(readings))
	}

	if exams[0].Code != msgtypes.NationalExam || readings[0].Code != msgtypes.Reading {
		t.Errorf("event codes = %v/%v, want NationalExam/Reading", exams[0].Code, readings[0].Code)
	}

	// Everything except the code must match, or the two readers have drifted.
	exams[0].Code, readings[0].Code = 0, 0
	if got, want := exams[0], readings[0]; !messageEqual(got, want) {
		t.Errorf("readers disagree on identical markup:\n national exam = %+v\n readings      = %+v", got, want)
	}
}

// TestEmitFlexTableAlignsValuesWithHeaders pins the pairing that the column
// shift broke: a <pre> note must be read as a whole cell (not a child span,
// which would drop it and slide later values left), and empty cells must stay
// as padding so Fields[i] keeps lining up with Descriptions[i].
func TestEmitFlexTableAlignsValuesWithHeaders(t *testing.T) {
	t.Parallel()

	doc := docFrom(t, flexTableHTML)

	ch := make(chan msgtypes.Message, 8)
	emitFlexTable(t.Context(), ch, doc, newTabScope(doc), selNationalExamTable, msgtypes.NationalExam, "u@skole.hr", "Matematika")
	close(ch)

	got := collect(ch)
	if len(got) != 1 {
		t.Fatalf("emitted %d messages, want 1 (the all-empty row must be skipped)", len(got))
	}

	wantDesc := []string{"Datum", "Ispit", "Bilješka"}
	wantFields := []string{"1.3.", "Matematika", "dobar rezultat"}

	if !slicesEqual(got[0].Descriptions, wantDesc) {
		t.Errorf("Descriptions = %q, want %q", got[0].Descriptions, wantDesc)
	}

	// The <pre> cell must survive and stay in column 3; a span-only read
	// yields ["1.3.", "Matematika"] and misfiles every later value.
	if !slicesEqual(got[0].Fields, wantFields) {
		t.Errorf("Fields = %q, want %q — a <pre> note dropped here shifts every later column", got[0].Fields, wantFields)
	}
}

// TestEmitFlexTableReportsCancellation checks that a cancelled context stops
// the emit and is reported, so parseCourse surfaces ctx.Err() rather than
// reporting a truncated scrape as a quiet school day.
func TestEmitFlexTableReportsCancellation(t *testing.T) {
	t.Parallel()

	doc := docFrom(t, flexTableHTML)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	// Unbuffered: the send cannot proceed, so the cancelled ctx wins the select.
	ch := make(chan msgtypes.Message)

	if !emitFlexTable(ctx, ch, doc, newTabScope(doc), selNationalExamTable, msgtypes.NationalExam, "u@skole.hr", "M") {
		t.Error("emitFlexTable() reported no cancellation on a cancelled context; parseCourse would return nil and hide a truncated scrape")
	}
}

// TestEmitFlexTableSkipsContentlessAndHeaderlessTables covers the two guards
// that keep an empty alert from reaching a messenger: a row whose cells are all
// blank, and a table carrying no header row to label values with.
func TestEmitFlexTableSkipsContentlessAndHeaderlessTables(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		html string
	}{
		{
			name: "every cell blank",
			html: `<div class="content"><div class="flex-table national-exam-table">
				<div class="row header"><div class="cell">Datum</div></div>
				<div class="row"><div class="cell">  </div></div></div></div>`,
		},
		{
			name: "no header row to pair against",
			html: `<div class="content"><div class="flex-table national-exam-table">
				<div class="row"><div class="cell">1.3.</div></div></div></div>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			doc := docFrom(t, tt.html)

			ch := make(chan msgtypes.Message, 4)
			emitFlexTable(t.Context(), ch, doc, newTabScope(doc), selNationalExamTable, msgtypes.NationalExam, "u@skole.hr", "M")
			close(ch)

			if got := collect(ch); len(got) != 0 {
				t.Errorf("emitted %d messages, want 0: %+v", len(got), got)
			}
		})
	}
}

// TestEmitFlexTableHonoursTabScope checks the school-year gate still applies
// through the shared reader: a table inside an inactive div.tab-content must
// not alert under the current class's name.
func TestEmitFlexTableHonoursTabScope(t *testing.T) {
	t.Parallel()

	const tabbed = `<div class="content">
	  <div class="tab-content active" data-schoolyear="2025/2026">
	    <div class="flex-table national-exam-table">
	      <div class="row header"><div class="cell">Datum</div></div>
	      <div class="row"><div class="cell">current</div></div>
	    </div>
	  </div>
	  <div class="tab-content" data-schoolyear="2024/2025">
	    <div class="flex-table national-exam-table">
	      <div class="row header"><div class="cell">Datum</div></div>
	      <div class="row"><div class="cell">past</div></div>
	    </div>
	  </div>
	</div>`

	doc := docFrom(t, tabbed)

	ch := make(chan msgtypes.Message, 8)
	emitFlexTable(t.Context(), ch, doc, newTabScope(doc), selNationalExamTable, msgtypes.NationalExam, "u@skole.hr", "M")
	close(ch)

	got := collect(ch)
	if len(got) != 1 {
		t.Fatalf("emitted %d messages, want 1 (only the active school year)", len(got))
	}

	if got[0].Fields[0] != "current" {
		t.Errorf("emitted the inactive school year's row (%q); a past closing grade would alert under the current class", got[0].Fields[0])
	}
}

// TestHeaderDescriptionsKeepsBlankPadding pins that blank header cells are
// retained rather than compacted. Dropping one would renumber every later
// column and silently pair values with the wrong label.
func TestHeaderDescriptionsKeepsBlankPadding(t *testing.T) {
	t.Parallel()

	const html = `<div class="flex-table">
	  <div class="row header first"><div class="cell">Naslov</div></div>
	  <div class="row header"><div class="cell">Datum</div><div class="cell">  </div><div class="cell">Ocjena</div></div>
	</div>`

	doc := docFrom(t, html)

	got := headerDescriptions(doc.Selection)
	want := []string{"Datum", "", "Ocjena"}

	if !slicesEqual(got, want) {
		t.Errorf("headerDescriptions() = %q, want %q — a compacted blank renumbers every later column", got, want)
	}
}

// TestHeaderDescriptionsNormalisesWhitespaceLikeCellValues checks the header
// reader and the value reader agree on whitespace handling. They pair by
// index, so a header normalised differently from its value reads as drift.
func TestHeaderDescriptionsNormalisesWhitespaceLikeCellValues(t *testing.T) {
	t.Parallel()

	const html = `<div class="flex-table">
	  <div class="row header first"><div class="cell">Naslov</div></div>
	  <div class="row header"><div class="cell"> Datum   ispita
	  </div></div>
	  <div class="row"><div class="cell"> Datum   ispita
	  </div></div>
	</div>`

	doc := docFrom(t, html)

	headers := headerDescriptions(doc.Selection)
	values, _ := cellValues(doc.FindMatcher(selRowNotHeader))

	if len(headers) != 1 || len(values) != 1 {
		t.Fatalf("got %d header(s) and %d value(s), want 1 each", len(headers), len(values))
	}

	if headers[0] != values[0] {
		t.Errorf("header %q and value %q normalised differently from identical markup", headers[0], values[0])
	}

	if headers[0] != "Datum ispita" {
		t.Errorf("headerDescriptions() = %q, want %q", headers[0], "Datum ispita")
	}
}

// slicesEqual reports whether two string slices hold the same values in order.
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// messageEqual compares the fields emitFlexTable populates.
func messageEqual(a, b msgtypes.Message) bool {
	return a.Code == b.Code &&
		a.Username == b.Username &&
		a.Subject == b.Subject &&
		slicesEqual(a.Fields, b.Fields) &&
		slicesEqual(a.Descriptions, b.Descriptions)
}
