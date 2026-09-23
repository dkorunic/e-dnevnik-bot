// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// goldenExam is dated far enough ahead that it never ages into "past".
func goldenExam() msgtypes.Message {
	return msgtypes.Message{
		Code:      msgtypes.Exam,
		Timestamp: time.Date(2099, 9, 24, 0, 0, 0, 0, time.UTC),
		Username:  "pero.peric",
		Subject:   "Matematika",
		Fields:    []string{"Matematika", "24.09.2099.", "Pisana provjera"},
	}
}

// TestExamEventOfGolden: both calendar backends key idempotency on this ID, so
// it must match what Google Calendar has always received
// (TestProcessCalendarEventIDGolden pins the same constant from the other side).
func TestExamEventOfGolden(t *testing.T) {
	t.Parallel()

	ev, ok := examEventOf("test", goldenExam())
	if !ok {
		t.Fatal("examEventOf rejected a valid future exam")
	}

	if want := "b1fc26040a20d97909464aeea740e92687c9c1db5c1ba79cee7e67db67678851"; ev.ID != want {
		t.Errorf("ID = %q, want %q", ev.ID, want)
	}

	if want := "pero.peric" + CalendarExamSep + "Matematika"; ev.Summary != want {
		t.Errorf("Summary = %q, want %q", ev.Summary, want)
	}

	if ev.Description != "Pisana provjera" {
		t.Errorf("Description = %q, want the note from Fields[2]", ev.Description)
	}

	if !ev.Date.Equal(goldenExam().Timestamp) {
		t.Errorf("Date = %v, want %v", ev.Date, goldenExam().Timestamp)
	}
}

// TestExamEventOfRejects: anything a calendar must not receive — grades in a
// calendar, past exams, contentless entries.
func TestExamEventOfRejects(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		mutate func(*msgtypes.Message)
	}{
		{name: "grade", mutate: func(g *msgtypes.Message) { g.Code = msgtypes.Grade }},
		{name: "reading", mutate: func(g *msgtypes.Message) { g.Code = msgtypes.Reading }},
		{name: "final grade", mutate: func(g *msgtypes.Message) { g.Code = msgtypes.FinalGrade }},
		{name: "national exam", mutate: func(g *msgtypes.Message) { g.Code = msgtypes.NationalExam }},
		{name: "yesterday", mutate: func(g *msgtypes.Message) {
			g.Timestamp = time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour)
		}},
		{name: "no fields", mutate: func(g *msgtypes.Message) { g.Fields = nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			g := goldenExam()
			tt.mutate(&g)

			if _, ok := examEventOf("test", g); ok {
				t.Errorf("examEventOf accepted %+v; a calendar must not receive it", g)
			}
		})
	}
}

// TestExamEventOfTodayIsAccepted: exam timestamps are midnight-UTC markers, so
// an instant comparison would drop an exam first seen on its own day.
func TestExamEventOfTodayIsAccepted(t *testing.T) {
	t.Parallel()

	g := goldenExam()
	g.Timestamp = time.Now().UTC().Truncate(24 * time.Hour)

	if _, ok := examEventOf("test", g); !ok {
		t.Error("examEventOf rejected an exam dated today")
	}
}

// TestExamEventOfShortFieldsNoDescription: legacy queue rows can carry fewer
// than three fields; they must yield no description, not a mis-picked field or
// a panic.
func TestExamEventOfShortFieldsNoDescription(t *testing.T) {
	t.Parallel()

	for _, fields := range [][]string{{"Matematika"}, {"Matematika", "24.09.2099."}} {
		g := goldenExam()
		g.Fields = fields

		ev, ok := examEventOf("test", g)
		if !ok {
			t.Fatalf("examEventOf rejected %v", fields)
		}

		if ev.Description != "" {
			t.Errorf("fields %v gave description %q, want empty", fields, ev.Description)
		}
	}
}
