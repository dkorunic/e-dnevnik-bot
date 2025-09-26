package format

import (
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

func TestPlainMsg(t *testing.T) {
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
