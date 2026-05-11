// @license
// Copyright (C) 2025  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT
// LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package scrape

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/fetch"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

func TestParseGrades(t *testing.T) {
	t.Parallel()
	html := `
	<div class="content">
		<div class="flex-table new-grades-table" data-action-id="Math">
			<div class="row header">
				<div class="cell"><span>Date</span></div>
				<div class="cell"><span>Grade</span></div>
			</div>
			<div class="row">
				<div class="cell"><span>01.01.2025.</span></div>
				<div class="cell"><span>5</span></div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 1)
	err := parseGrades(context.Background(), ch, "testuser", []byte(html), false, "ClassA")
	if err != nil {
		t.Fatalf("parseGrades failed: %v", err)
	}
	close(ch)

	msg := <-ch
	expected := msgtypes.Message{
		Code:         msgtypes.Grade,
		Username:     "testuser",
		Subject:      "Math",
		Descriptions: []string{"Date", "Grade"},
		Fields:       []string{"01.01.2025.", "5"},
	}

	if !reflect.DeepEqual(msg, expected) {
		t.Errorf("Expected %+v, got %+v", expected, msg)
	}
}

func TestParseEvents(t *testing.T) {
	t.Parallel()
	events := fetch.Events{
		{
			Summary:     "History: Test",
			Description: "Chapter 1",
			Start:       time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
		},
	}

	ch := make(chan msgtypes.Message, 1)
	err := parseEvents(context.Background(), ch, "testuser", events, false, "ClassA")
	if err != nil {
		t.Fatalf("parseEvents failed: %v", err)
	}
	close(ch)

	msg := <-ch
	expected := msgtypes.Message{
		Code:     msgtypes.Exam,
		Username: "testuser",
		Subject:  "Test",
		Descriptions: []string{
			EventSummary,
			DateDescription,
			EventDescription,
		},
		Fields:    []string{"Test", "01.01.2025.", "Chapter 1"},
		Timestamp: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	// .Equal handles tz/monotonic differences DeepEqual cannot.
	if !msg.Timestamp.Equal(expected.Timestamp) {
		t.Errorf("Timestamp mismatch: got %v, want %v", msg.Timestamp, expected.Timestamp)
	}

	msg.Timestamp = time.Time{}
	expected.Timestamp = time.Time{}

	if !reflect.DeepEqual(msg, expected) {
		t.Errorf("Expected %+v, got %+v", expected, msg)
	}
}

func TestParseClasses(t *testing.T) {
	t.Parallel()
	html := `
	<div class="student-list">
		<div class="classes">
			<div class="class-menu-vertical">
				<div class="class-info" data-action-id="123">
					<div class="class"><span class="bold">ClassA</span><span class="class-schoolyear">2024/2025</span></div>
					<div class="school"><div><span class="school-name">Test School</span></div></div>
				</div>
			</div>
		</div>
	</div>`

	classes, err := parseClasses("testuser", []byte(html))
	if err != nil {
		t.Fatalf("parseClasses failed: %v", err)
	}

	expected := fetch.Classes{
		{
			ID:     "123",
			Name:   "ClassA",
			Year:   "2024/2025",
			School: "Test School",
		},
	}

	if !reflect.DeepEqual(classes, expected) {
		t.Errorf("Expected %+v, got %+v", expected, classes)
	}
}

func TestParseCourses(t *testing.T) {
	t.Parallel()
	html := `
	<div class="content">
		<ul class="list">
			<li><a href="/course/1"><div class="course-info"><span>Physics</span></div></a></li>
		</ul>
	</div>`

	courses, err := parseCourses([]byte(html))
	if err != nil {
		t.Fatalf("parseCourses failed: %v", err)
	}

	expected := fetch.Courses{
		{
			Name: "Physics",
			URL:  "/course/1",
		},
	}

	if !reflect.DeepEqual(courses, expected) {
		t.Errorf("Expected %+v, got %+v", expected, courses)
	}
}

func TestParseCourse(t *testing.T) {
	t.Parallel()
	html := `
	<div class="content">
		<div class="flex-table readings-table">
			<div class="row header">
				<div class="cell"><span>Author</span></div>
				<div class="cell"><span>Title</span></div>
			</div>
			<div class="row">
				<div class="cell"><span>J.K. Rowling</span></div>
				<div class="cell"><span>Harry Potter</span></div>
			</div>
		</div>
		<div class="flex-table s grades-table">
			<div class="row final-grade">
				<div class="cell bold first"><span>Final Grade</span></div>
				<div class="cell"><span>5</span></div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 2)
	err := parseCourse(context.Background(), ch, "testuser", []byte(html), false, "ClassA", "English")
	if err != nil {
		t.Fatalf("parseCourse failed: %v", err)
	}
	close(ch)

	var msgs []msgtypes.Message
	for msg := range ch {
		msgs = append(msgs, msg)
	}

	expected := []msgtypes.Message{
		{
			Code:         msgtypes.Reading,
			Username:     "testuser",
			Subject:      "English",
			Descriptions: []string{"Author", "Title"},
			Fields:       []string{"J.K. Rowling", "Harry Potter"},
		},
		{
			Code:         msgtypes.FinalGrade,
			Username:     "testuser",
			Subject:      "English / ClassA",
			Descriptions: []string{"Final Grade"},
			Fields:       []string{"5"},
		},
	}

	if len(msgs) != len(expected) {
		t.Fatalf("Expected %d messages, got %d", len(expected), len(msgs))
	}

	// Match by Code so the assertion survives any future emission-order change.
	byCode := make(map[msgtypes.EventCode]msgtypes.Message, len(msgs))
	for _, m := range msgs {
		byCode[m.Code] = m
	}

	for _, exp := range expected {
		got, ok := byCode[exp.Code]
		if !ok {
			t.Errorf("Expected message with code %v, none found", exp.Code)

			continue
		}

		if !reflect.DeepEqual(got, exp) {
			t.Errorf("Expected %+v, got %+v", exp, got)
		}
	}
}

func TestParseGradesMultiClass(t *testing.T) {
	t.Parallel()
	html := `
	<div class="content">
		<div class="flex-table new-grades-table" data-action-id="Physics">
			<div class="row header">
				<div class="cell"><span>Date</span></div>
				<div class="cell"><span>Grade</span></div>
			</div>
			<div class="row">
				<div class="cell"><span>15.03.2025.</span></div>
				<div class="cell"><span>4</span></div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 1)

	err := parseGrades(context.Background(), ch, "testuser", []byte(html), true, "ClassB")
	if err != nil {
		t.Fatalf("parseGrades failed: %v", err)
	}

	close(ch)

	msg := <-ch
	expectedSubject := "Physics / ClassB"

	if msg.Subject != expectedSubject {
		t.Errorf("expected subject %q, got %q", expectedSubject, msg.Subject)
	}
}

func TestParseEventsEmpty(t *testing.T) {
	t.Parallel()
	ch := make(chan msgtypes.Message, 1)

	err := parseEvents(context.Background(), ch, "testuser", fetch.Events{}, false, "ClassA")
	if err != nil {
		t.Fatalf("parseEvents failed: %v", err)
	}

	close(ch)

	if len(ch) != 0 {
		t.Error("parseEvents() with empty events should send nothing to channel")
	}
}

func TestParseEventsMultiClass(t *testing.T) {
	t.Parallel()
	events := fetch.Events{
		{
			Summary:     "Math: Test",
			Description: "Algebra",
			Start:       time.Date(2025, 6, 10, 0, 0, 0, 0, time.UTC),
		},
	}

	ch := make(chan msgtypes.Message, 1)

	err := parseEvents(context.Background(), ch, "testuser", events, true, "ClassC")
	if err != nil {
		t.Fatalf("parseEvents failed: %v", err)
	}

	close(ch)

	msg := <-ch
	expectedSubject := "Test / ClassC"

	if msg.Subject != expectedSubject {
		t.Errorf("expected subject %q, got %q", expectedSubject, msg.Subject)
	}
}

func TestParseCourseNationalExam(t *testing.T) {
	t.Parallel()
	html := `
	<div class="content">
		<div class="flex-table national-exam-table">
			<div class="row header first">
				<div class="cell"><span>Block</span></div>
			</div>
			<div class="row header">
				<div class="cell"><span>Subject</span></div>
				<div class="cell"><span>Score</span></div>
			</div>
			<div class="row">
				<div class="cell"><span>Mathematics</span></div>
				<div class="cell"><span>85</span></div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 2)

	err := parseCourse(context.Background(), ch, "testuser", []byte(html), false, "ClassA", "Maths")
	if err != nil {
		t.Fatalf("parseCourse failed: %v", err)
	}

	close(ch)

	var msgs []msgtypes.Message
	for msg := range ch {
		msgs = append(msgs, msg)
	}

	if len(msgs) == 0 {
		t.Fatal("parseCourse() should have returned at least 1 national exam message")
	}

	if msgs[0].Code != msgtypes.NationalExam {
		t.Errorf("expected NationalExam code, got %v", msgs[0].Code)
	}
}

func TestParseCourseMultiClass(t *testing.T) {
	t.Parallel()
	html := `
	<div class="content">
		<div class="flex-table readings-table">
			<div class="row header">
				<div class="cell"><span>Author</span></div>
				<div class="cell"><span>Title</span></div>
			</div>
			<div class="row">
				<div class="cell"><span>Author Name</span></div>
				<div class="cell"><span>Book Title</span></div>
			</div>
		</div>
	</div>`

	ch := make(chan msgtypes.Message, 2)

	err := parseCourse(context.Background(), ch, "testuser", []byte(html), true, "ClassD", "Literature")
	if err != nil {
		t.Fatalf("parseCourse failed: %v", err)
	}

	close(ch)

	var msgs []msgtypes.Message
	for msg := range ch {
		msgs = append(msgs, msg)
	}

	if len(msgs) == 0 {
		t.Fatal("parseCourse() should return at least 1 reading message")
	}

	expectedSubject := "Literature / ClassD"
	if msgs[0].Subject != expectedSubject {
		t.Errorf("expected subject %q, got %q", expectedSubject, msgs[0].Subject)
	}
}

func TestParseClassesEmpty(t *testing.T) {
	t.Parallel()
	html := `<div class="student-list"><div class="classes"></div></div>`

	classes, err := parseClasses("testuser", []byte(html))
	if err != nil {
		t.Fatalf("parseClasses failed: %v", err)
	}

	if len(classes) != 0 {
		t.Errorf("expected 0 classes, got %d", len(classes))
	}
}

func TestTrimAllSpace(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"leading space", "  hello", "hello"},
		{"trailing space", "hello  ", "hello"},
		{"multiple spaces", "hello   world", "hello world"},
		{"newlines", "hello\nworld", "hello world"},
		{"mixed", "  hello \n world  ", "hello world"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := trimAllSpace(tt.input); got != tt.want {
				t.Errorf("trimAllSpace() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCleanEventDescription(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"with colon", "Subject: Event", "Event"},
		{"no colon", "Event", "Event"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := cleanEventDescription(tt.input); got != tt.want {
				t.Errorf("cleanEventDescription() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCleanEventDescriptionReturnAfterNotBefore verifies Bug 2 from TESTING-PLAN:
// strings.Cut returns (before, after, found); the function must return the AFTER
// portion. A mutation that swaps to returning `before` would return the subject
// label instead of the description, causing all exam events to re-alert.
func TestCleanEventDescriptionReturnAfterNotBefore(t *testing.T) {
	t.Parallel()

	input := "Matematika: Pisanje testa"
	want := "Pisanje testa"

	got := cleanEventDescription(input)
	if got != want {
		t.Errorf("cleanEventDescription(%q) = %q, want %q (got before instead of after?)", input, got, want)
	}

	if got == "Matematika" {
		t.Errorf("cleanEventDescription returned the 'before' part (subject label) instead of the 'after' part (description)")
	}
}

// TestTrimAllSpaceConcurrent verifies Bug 3C from TESTING-PLAN:
// the pool builder must NOT be returned before b.String() is called.
// Run under -race to detect the data race that would occur if the order were reversed.
func TestTrimAllSpaceConcurrent(t *testing.T) {
	t.Parallel()

	const goroutines = 50

	var wg sync.WaitGroup

	wg.Add(goroutines)

	for range goroutines {
		go func() {
			defer wg.Done()

			// Inputs need normalisation so pass 2 is always taken (race-detector target).
			inputs := []string{"  hello world  ", "foo  bar", "\nhello\nworld\n", "  a  b  c  "}
			for _, s := range inputs {
				_ = trimAllSpace(s)
			}
		}()
	}

	wg.Wait()
}

// TestParseEventsFieldOrder verifies Bug 5 from TESTING-PLAN:
// Fields must be ordered [subject, date, description], matching the parallel
// Descriptions slice ["Predmet", "Datum ispita", "Napomena"].
// A mutation swapping subject and timestamp in Fields would be caught here.
func TestParseEventsFieldOrder(t *testing.T) {
	t.Parallel()

	events := fetch.Events{
		{
			Summary:     "Matematika: Pisanje testa",
			Description: "Algebra poglavlje 3",
			Start:       time.Date(2025, 6, 12, 0, 0, 0, 0, time.UTC),
		},
	}

	ch := make(chan msgtypes.Message, 1)

	if err := parseEvents(context.Background(), ch, "testuser", events, false, "ClassA"); err != nil {
		t.Fatalf("parseEvents failed: %v", err)
	}

	close(ch)

	msg := <-ch

	if len(msg.Fields) < 3 {
		t.Fatalf("expected at least 3 fields, got %d", len(msg.Fields))
	}

	if msg.Fields[0] != "Pisanje testa" {
		t.Errorf("Fields[0] = %q, want %q (subject name)", msg.Fields[0], "Pisanje testa")
	}

	if msg.Fields[1] != "12.06.2025." {
		t.Errorf("Fields[1] = %q, want %q (formatted date)", msg.Fields[1], "12.06.2025.")
	}

	if msg.Fields[2] != "Algebra poglavlje 3" {
		t.Errorf("Fields[2] = %q, want %q (description)", msg.Fields[2], "Algebra poglavlje 3")
	}
}

// TestParseCourseDistinctFinalGradeSubjectPerClass verifies Bug 4 from TESTING-PLAN:
// single-class final grades must include the class name in Subject so that
// grades from different school years with the same subject name produce
// distinct database hashes. Removing the "if !multiClass" suffix block
// would make both FinalGrade Messages have identical Subject fields.
func TestParseCourseDistinctFinalGradeSubjectPerClass(t *testing.T) {
	t.Parallel()

	html := `
	<div class="content">
		<div class="flex-table s grades-table">
			<div class="row final-grade">
				<div class="cell bold first"><span>Zaključna ocjena</span></div>
				<div class="cell"><span>5</span></div>
			</div>
		</div>
	</div>`

	ch1 := make(chan msgtypes.Message, 2)

	if err := parseCourse(context.Background(), ch1, "testuser", []byte(html), false, "ClassA", "Matematika"); err != nil {
		t.Fatalf("parseCourse ClassA failed: %v", err)
	}

	close(ch1)

	ch2 := make(chan msgtypes.Message, 2)

	if err := parseCourse(context.Background(), ch2, "testuser", []byte(html), false, "ClassB", "Matematika"); err != nil {
		t.Fatalf("parseCourse ClassB failed: %v", err)
	}

	close(ch2)

	var subjectA, subjectB string

	for msg := range ch1 {
		if msg.Code == msgtypes.FinalGrade {
			subjectA = msg.Subject
		}
	}

	for msg := range ch2 {
		if msg.Code == msgtypes.FinalGrade {
			subjectB = msg.Subject
		}
	}

	if subjectA == "" || subjectB == "" {
		t.Fatal("expected FinalGrade messages from both parseCourse calls")
	}

	if subjectA == subjectB {
		t.Errorf("FinalGrade subjects must differ per class; both returned %q", subjectA)
	}
}
