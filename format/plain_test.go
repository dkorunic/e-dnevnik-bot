package format

import (
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

func TestPlainMsg(t *testing.T) {
	t.Parallel()
	username := "testuser"
	subject := "Test Subject"
	code := msgtypes.Grade
	descriptions := []string{"desc1", "desc2"}
	grade := []string{"grade1", "grade2"}

	expected := GradePrefix + "testuser / Test Subject\n\ndesc1: grade1\ndesc2: grade2\n"
	result := PlainMsg(username, subject, code, descriptions, grade)

	if result != expected {
		t.Errorf("PlainMsg() = %q, want %q", result, expected)
	}
}

func TestPlainFormatGrades(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	descriptions := []string{"desc1", "desc2"}
	grade := []string{"grade1", "grade2"}

	plainFormatGrades(&sb, descriptions, grade)

	expected := "desc1: grade1\ndesc2: grade2\n"
	result := sb.String()

	if result != expected {
		t.Errorf("plainFormatGrades() = %q, want %q", result, expected)
	}
}

func TestPlainFormatSubject(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	user := "testuser"
	subject := "Test Subject"
	code := msgtypes.Exam

	PlainFormatSubject(&sb, user, subject, code)

	expected := ExamPrefix + "testuser / Test Subject"
	result := sb.String()

	if result != expected {
		t.Errorf("PlainFormatSubject() = %q, want %q", result, expected)
	}
}

func TestPlainAddHeader(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	user := "testuser"
	subject := "Test Subject"
	code := msgtypes.Reading

	plainAddHeader(&sb, user, subject, code)

	expected := ReadingPrefix + "testuser / Test Subject\n\n"
	result := sb.String()

	if result != expected {
		t.Errorf("plainAddHeader() = %q, want %q", result, expected)
	}
}

// TestPlainFormatSubjectAllCodes verifies Bug 10 from TESTING-PLAN:
// each EventCode must map to its own distinct prefix. A mutation swapping the
// Grade and Exam cases would make grades display the exam prefix and vice versa.
func TestPlainFormatSubjectAllCodes(t *testing.T) {
	t.Parallel()

	cases := []struct {
		code   msgtypes.EventCode
		prefix string
	}{
		{msgtypes.Grade, GradePrefix},
		{msgtypes.Exam, ExamPrefix},
		{msgtypes.Reading, ReadingPrefix},
		{msgtypes.FinalGrade, FinalGradePrefix},
		{msgtypes.NationalExam, NationalExamPrefix},
	}

	for _, tc := range cases {
		t.Run(tc.prefix, func(t *testing.T) {
			t.Parallel()

			var sb strings.Builder

			PlainFormatSubject(&sb, "u", "s", tc.code)
			got := sb.String()

			if !strings.HasPrefix(got, tc.prefix) {
				t.Errorf("PlainFormatSubject code %v: got %q, want prefix %q (Grade/Exam prefix swapped?)", tc.code, got, tc.prefix)
			}

			// Explicitly verify no other prefix appears at the start.
			for _, other := range cases {
				if other.code != tc.code && strings.HasPrefix(got, other.prefix) {
					t.Errorf("PlainFormatSubject code %v: got prefix %q, which belongs to code %v", tc.code, other.prefix, other.code)
				}
			}
		})
	}
}
