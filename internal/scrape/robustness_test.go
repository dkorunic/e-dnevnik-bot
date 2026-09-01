// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/fetch"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// drain collects everything buffered on ch without blocking.
func drain(ch chan msgtypes.Message) []msgtypes.Message {
	var out []msgtypes.Message

	for {
		select {
		case m := <-ch:
			out = append(out, m)
		default:
			return out
		}
	}
}

// TestParseGradesSkipsMalformedTables covers the guards that let a partially
// broken page still yield the rows it can. The portal's HTML is not a contract:
// a table without data-action-id, or a row with no cells, must be skipped
// rather than emitting a subject-less or field-less alert.
func TestParseGradesSkipsMalformedTables(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		html string
	}{
		{
			name: "table without data-action-id has no subject",
			html: `<div class="content">
				<div class="flex-table new-grades-table">
					<div class="row header"><div class="cell"><span>Date</span></div></div>
					<div class="row"><div class="cell"><span>01.01.2025.</span></div></div>
				</div>
			</div>`,
		},
		{
			name: "row with no cells yields no fields",
			html: `<div class="content">
				<div class="flex-table new-grades-table" data-action-id="Matematika">
					<div class="row header"><div class="cell"><span>Date</span></div></div>
					<div class="row"></div>
				</div>
			</div>`,
		},
		{
			name: "header without cells yields no descriptions",
			html: `<div class="content">
				<div class="flex-table new-grades-table" data-action-id="Matematika">
					<div class="row header"></div>
					<div class="row"><div class="cell"><span>5</span></div></div>
				</div>
			</div>`,
		},
		{
			name: "no grades table at all",
			html: `<div class="content"><p>Nema ocjena</p></div>`,
		},
		{
			name: "empty document",
			html: ``,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ch := make(chan msgtypes.Message, 8)

			if err := parseGrades(t.Context(), ch, "testuser", []byte(tt.html), false, "ClassA"); err != nil {
				t.Fatalf("parseGrades() = %v, want nil — a malformed page must be skipped, not fail the cycle", err)
			}

			if got := drain(ch); len(got) != 0 {
				t.Errorf("parseGrades emitted %+v, want nothing for malformed input", got)
			}
		})
	}
}

// TestParseGradesCancelledContext: parsing must abandon a page mid-way when the
// context is cancelled and report ctx.Err(). Without the select, a full channel
// during shutdown would block the scraper goroutine past the poll cycle.
func TestParseGradesCancelledContext(t *testing.T) {
	t.Parallel()

	html := `<div class="content">
		<div class="flex-table new-grades-table" data-action-id="Matematika">
			<div class="row header"><div class="cell"><span>Datum</span></div></div>
			<div class="row"><div class="cell"><span>01.01.2025.</span></div></div>
			<div class="row"><div class="cell"><span>02.01.2025.</span></div></div>
		</div>
	</div>`

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// Unbuffered: the send can only proceed via the ctx.Done() arm.
	ch := make(chan msgtypes.Message)

	err := parseGrades(ctx, ch, "testuser", []byte(html), false, "ClassA")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("parseGrades() = %v, want context.Canceled", err)
	}
}

// TestParseCourseCancelledContext is the same guard on the course parser, which
// walks national-exam, readings and final-grade tables.
func TestParseCourseCancelledContext(t *testing.T) {
	t.Parallel()

	html := `<div class="content">
		<div class="flex-table national-exam-table">
			<div class="row header"><div class="cell"><span>Ispit</span></div></div>
			<div class="row"><div class="cell"><span>Matematika A</span></div></div>
		</div>
	</div>`

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch := make(chan msgtypes.Message)

	err := parseCourse(ctx, ch, "testuser", []byte(html), false, "ClassA", "Matematika")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("parseCourse() = %v, want context.Canceled", err)
	}
}

// TestParseEventsCancelledContext: exam events come from the ICS feed and are
// forwarded one by one, so the same cancellation guard applies.
func TestParseEventsCancelledContext(t *testing.T) {
	t.Parallel()

	events := fetch.Events{
		{Summary: "Matematika: Ispit", Description: "Cijeli gradivo", Start: time.Now()},
		{Summary: "Fizika: Ispit", Description: "Poglavlje 3", Start: time.Now()},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch := make(chan msgtypes.Message)

	err := parseEvents(ctx, ch, "testuser", events, false, "ClassA")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("parseEvents() = %v, want context.Canceled", err)
	}
}

// TestParseClassesSkipsEntriesWithoutID: the class ID is interpolated into the
// class-switch URL, so an entry without data-action-id is unusable and must be
// dropped rather than produce a class with an empty ID.
func TestParseClassesSkipsEntriesWithoutID(t *testing.T) {
	t.Parallel()

	html := `<div class="student-list"><div class="classes">
		<div class="class-menu-vertical">
			<div class="class-info">
				<div class="class"><span class="bold">Bez IDa</span></div>
			</div>
		</div>
		<div class="class-menu-vertical">
			<div class="class-info" data-action-id="123">
				<div class="class"><span class="bold">ClassA</span><span class="class-schoolyear">2024/2025</span></div>
				<div class="school"><div><span class="school-name">Test School</span></div></div>
			</div>
		</div>
	</div></div>`

	classes, err := parseClasses("testuser", []byte(html))
	if err != nil {
		t.Fatalf("parseClasses() = %v, want nil", err)
	}

	if len(classes) != 1 {
		t.Fatalf("parseClasses returned %+v, want only the entry carrying data-action-id", classes)
	}

	if classes[0].ID != "123" {
		t.Errorf("class ID = %q, want %q", classes[0].ID, "123")
	}
}

// TestParseClassesToleratesDuplicateSpans covers the HTML-drift warnings: if the
// portal starts rendering more than one name/year/school span per class, the
// parser must take the first and carry on rather than concatenating them into
// one unreadable string or dropping the class.
func TestParseClassesToleratesDuplicateSpans(t *testing.T) {
	t.Parallel()

	html := `<div class="student-list"><div class="classes">
		<div class="class-menu-vertical">
			<div class="class-info" data-action-id="456">
				<div class="class">
					<span class="bold">ClassA</span>
					<span class="bold">ClassA duplicate</span>
					<span class="class-schoolyear">2024/2025</span>
					<span class="class-schoolyear">2025/2026</span>
				</div>
				<div class="school">
					<div><span class="school-name">Prva skola</span></div>
					<div><span class="school-name">Druga skola</span></div>
				</div>
			</div>
		</div>
	</div></div>`

	classes, err := parseClasses("testuser", []byte(html))
	if err != nil {
		t.Fatalf("parseClasses() = %v, want nil", err)
	}

	if len(classes) != 1 {
		t.Fatalf("parseClasses returned %d classes, want 1", len(classes))
	}

	got := classes[0]
	if got.Name != "ClassA" {
		t.Errorf("Name = %q, want the first span %q", got.Name, "ClassA")
	}

	if got.Year != "2024/2025" {
		t.Errorf("Year = %q, want the first span %q", got.Year, "2024/2025")
	}

	if got.School != "Prva skola" {
		t.Errorf("School = %q, want the first span %q", got.School, "Prva skola")
	}
}

// TestParseClassesSkipsPastSchoolYears: the selector deliberately excludes
// past-schoolyear blocks. Including them would re-scrape and re-alert on the
// previous year's grades every cycle.
func TestParseClassesSkipsPastSchoolYears(t *testing.T) {
	t.Parallel()

	html := `<div class="student-list"><div class="classes">
		<div class="class-menu-vertical past-schoolyear">
			<div class="class-info" data-action-id="old">
				<div class="class"><span class="bold">Prosla godina</span></div>
			</div>
		</div>
		<div class="class-menu-vertical">
			<div class="class-info" data-action-id="current">
				<div class="class"><span class="bold">Ova godina</span></div>
			</div>
		</div>
	</div></div>`

	classes, err := parseClasses("testuser", []byte(html))
	if err != nil {
		t.Fatalf("parseClasses() = %v, want nil", err)
	}

	for _, c := range classes {
		if c.ID == "old" {
			t.Errorf("parseClasses returned a past school year (%+v); those would re-alert every cycle", c)
		}
	}

	if len(classes) != 1 || classes[0].ID != "current" {
		t.Errorf("parseClasses returned %+v, want only the current school year", classes)
	}
}

// TestParseCoursesSkipsLinksWithoutHref: the href is the only way to fetch a
// course, so an anchor without one must be dropped rather than yielding a
// Course whose empty URL would later resolve to the portal root.
func TestParseCoursesSkipsLinksWithoutHref(t *testing.T) {
	t.Parallel()

	html := `<div class="content"><ul class="list">
		<li><a><div class="course-info"><span>Bez linka</span><span>Nastavnik</span></div></a></li>
		<li><a href="/course/123"><div class="course-info"><span>Matematika</span><span>Nastavnik</span></div></a></li>
	</ul></div>`

	courses, err := parseCourses([]byte(html))
	if err != nil {
		t.Fatalf("parseCourses() = %v, want nil", err)
	}

	for _, c := range courses {
		if c.URL == "" {
			t.Errorf("parseCourses returned a course with an empty URL (%+v); it would fetch the portal root", c)
		}
	}

	if len(courses) != 1 || courses[0].Name != "Matematika" {
		t.Errorf("parseCourses returned %+v, want only the linked course", courses)
	}
}

// TestParseCoursesToleratesExtraInfoSpans: the course-info block normally holds
// exactly two spans (name, teacher). Extra spans must not corrupt the name.
func TestParseCoursesToleratesExtraInfoSpans(t *testing.T) {
	t.Parallel()

	html := `<div class="content"><ul class="list">
		<li><a href="/course/123"><div class="course-info">
			<span>Matematika</span><span>Nastavnik</span><span>Visak</span><span>Jos jedan</span>
		</div></a></li>
	</ul></div>`

	courses, err := parseCourses([]byte(html))
	if err != nil {
		t.Fatalf("parseCourses() = %v, want nil", err)
	}

	if len(courses) != 1 {
		t.Fatalf("parseCourses returned %d courses, want 1", len(courses))
	}

	if courses[0].Name != "Matematika" {
		t.Errorf("Name = %q, want the first span %q", courses[0].Name, "Matematika")
	}
}

// TestParseCourseSkipsContentlessRows mirrors the grades guard: a national-exam
// row with no spans, or a table with no header, must not produce an alert with
// empty fields.
func TestParseCourseSkipsContentlessRows(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		html string
	}{
		{
			name: "row without cells",
			html: `<div class="content"><div class="flex-table national-exam-table">
				<div class="row header"><div class="cell"><span>Ispit</span></div></div>
				<div class="row"></div>
			</div></div>`,
		},
		{
			name: "table without a header row",
			html: `<div class="content"><div class="flex-table national-exam-table">
				<div class="row"><div class="cell"><span>Matematika A</span></div></div>
			</div></div>`,
		},
		{
			name: "no recognised tables",
			html: `<div class="content"><p>Nema sadrzaja</p></div>`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ch := make(chan msgtypes.Message, 8)

			if err := parseCourse(t.Context(), ch, "testuser", []byte(tt.html), false, "ClassA", "Matematika"); err != nil {
				t.Fatalf("parseCourse() = %v, want nil", err)
			}

			for _, m := range drain(ch) {
				if len(m.Fields) == 0 || len(m.Descriptions) == 0 {
					t.Errorf("parseCourse emitted a contentless message: %+v", m)
				}
			}
		})
	}
}

// TestTrimPutBuilderDropsOversizedBuilders covers the pool guard: a builder
// grown past the cap is dropped instead of returned, so one outlier page cannot
// pin a large buffer in the pool for the process lifetime. The observable
// contract is that trimming still works correctly afterwards.
func TestTrimPutBuilderDropsOversizedBuilders(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("a b ", trimMaxPooledBuilderCap)

	got := trimAllSpace(huge)
	if strings.Contains(got, "  ") {
		t.Error("trimAllSpace left a double space in an oversized input")
	}

	// The pool must still be usable for normal input afterwards.
	if got := trimAllSpace("  Matematika   5  "); got != "Matematika 5" {
		t.Errorf("trimAllSpace() = %q after an oversized call, want %q", got, "Matematika 5")
	}
}
