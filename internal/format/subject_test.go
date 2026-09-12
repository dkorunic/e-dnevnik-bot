// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"strings"
	"sync"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// TestPlainSubject covers the pooled-builder wrapper around PlainFormatSubject,
// used for per-message embed titles.
func TestPlainSubject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		user    string
		subject string
		code    msgtypes.EventCode
		want    string
	}{
		{
			name:    "grade",
			user:    "pero.peric",
			subject: "Matematika",
			code:    msgtypes.Grade,
			want:    GradePrefix + "pero.peric / Matematika",
		},
		{
			name:    "exam",
			user:    "pero.peric",
			subject: "Fizika",
			code:    msgtypes.Exam,
			want:    ExamPrefix + "pero.peric / Fizika",
		},
		{
			name:    "reading",
			user:    "pero.peric",
			subject: "Hrvatski jezik",
			code:    msgtypes.Reading,
			want:    ReadingPrefix + "pero.peric / Hrvatski jezik",
		},
		{
			name:    "final grade",
			user:    "pero.peric",
			subject: "Kemija",
			code:    msgtypes.FinalGrade,
			want:    FinalGradePrefix + "pero.peric / Kemija",
		},
		{
			name:    "national exam",
			user:    "pero.peric",
			subject: "Biologija",
			code:    msgtypes.NationalExam,
			want:    NationalExamPrefix + "pero.peric / Biologija",
		},
		{
			name:    "empty user and subject still renders the prefix",
			user:    "",
			subject: "",
			code:    msgtypes.Grade,
			want:    GradePrefix + " / ",
		},
		{
			name:    "diacritics are preserved verbatim",
			user:    "ivan.horvat",
			subject: "Priroda i društvo",
			code:    msgtypes.Grade,
			want:    GradePrefix + "ivan.horvat / Priroda i društvo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := PlainSubject(tt.user, tt.subject, tt.code); got != tt.want {
				t.Errorf("PlainSubject() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPlainSubjectMatchesFormatter checks the pooled wrapper stays in step with
// the builder-based formatter it delegates to. If the two ever diverge, embed
// titles would silently stop matching message headers.
func TestPlainSubjectMatchesFormatter(t *testing.T) {
	t.Parallel()

	for _, code := range []msgtypes.EventCode{
		msgtypes.Grade, msgtypes.Exam, msgtypes.Reading, msgtypes.FinalGrade, msgtypes.NationalExam,
	} {
		var sb strings.Builder

		PlainFormatSubject(&sb, "u", "s", code)

		if got := PlainSubject("u", "s", code); got != sb.String() {
			t.Errorf("code %v: PlainSubject() = %q, PlainFormatSubject wrote %q", code, got, sb.String())
		}
	}
}

// TestPlainSubjectResultsAreIndependent: String() aliases the builder's buffer,
// so reusing a builder across calls would clobber results the caller still
// holds. Run with -race for the concurrent half to mean anything.
func TestPlainSubjectResultsAreIndependent(t *testing.T) {
	t.Parallel()

	// Sequential: hold every result, then re-check after many pool cycles.
	const n = 200

	got := make([]string, n)
	want := make([]string, n)

	for i := range n {
		subject := strings.Repeat("x", i%64+1)
		got[i] = PlainSubject("user", subject, msgtypes.Grade)
		want[i] = GradePrefix + "user / " + subject
	}

	for i := range n {
		if got[i] != want[i] {
			t.Fatalf("result %d was corrupted by a later call: got %q, want %q", i, got[i], want[i])
		}
	}

	// Concurrent: many goroutines formatting at once.
	var wg sync.WaitGroup

	for range 16 {
		wg.Go(func() {
			for i := range 100 {
				subject := strings.Repeat("y", i%32+1)

				if s := PlainSubject("user", subject, msgtypes.Exam); s != ExamPrefix+"user / "+subject {
					t.Errorf("concurrent PlainSubject() = %q, want %q", s, ExamPrefix+"user / "+subject)

					return
				}
			}
		})
	}

	wg.Wait()
}

// oversizedInput exceeds the formatters' Grow hint, forcing a mid-render realloc.
const oversizedInput = 128 * 1024

// TestPlainSubjectOversizedInput: an oversized subject must render whole and
// leave the next call unaffected.
func TestPlainSubjectOversizedInput(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("z", oversizedInput)

	got := PlainSubject("user", huge, msgtypes.Grade)
	if got != GradePrefix+"user / "+huge {
		t.Errorf("PlainSubject() mangled an oversized subject (len %d)", len(got))
	}

	if s := PlainSubject("user", "Matematika", msgtypes.Grade); s != GradePrefix+"user / Matematika" {
		t.Errorf("PlainSubject() after an oversized call = %q, want the normal rendering", s)
	}
}
