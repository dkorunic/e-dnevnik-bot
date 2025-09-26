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
	"reflect"
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
	err := parseGrades(ch, "testuser", html, false, "ClassA")
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
	err := parseEvents(ch, "testuser", events, false, "ClassA")
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

	// Zero out the timestamp for comparison as it's not set in the expected message
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

	classes, err := parseClasses("testuser", html)
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

	courses, err := parseCourses(html)
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
	err := parseCourse(ch, "testuser", html, false, "ClassA", "English")
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

	// Sort for consistent comparison
	for i := range msgs {
		if msgs[i].Code == msgtypes.FinalGrade {
			msgs[i], msgs[0] = msgs[0], msgs[i]
			break
		}
	}

	if !reflect.DeepEqual(msgs[1], expected[0]) {
		t.Errorf("Expected %+v, got %+v", expected[0], msgs[1])
	}

	if !reflect.DeepEqual(msgs[0], expected[1]) {
		t.Errorf("Expected %+v, got %+v", expected[1], msgs[0])
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
