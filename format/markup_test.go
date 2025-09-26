package format

import (
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

func TestMarkupMsg(t *testing.T) {
	username := "testuser"
	subject := "Test Subject"
	code := msgtypes.Grade
	descriptions := []string{"desc1", "desc2"}
	grade := []string{"grade1", "grade2"}

	expected := "*" + GradePrefix + "testuser / Test Subject*\n\n```\ndesc1: grade1\ndesc2: grade2\n```\n"
	result := MarkupMsg(username, subject, code, descriptions, grade)

	if result != expected {
		t.Errorf("MarkupMsg() = %q, want %q", result, expected)
	}
}

func TestMarkupAddHeader(t *testing.T) {
	var sb strings.Builder
	user := "testuser"
	subject := "Test Subject"
	code := msgtypes.Exam

	markupAddHeader(&sb, user, subject, code)

	expected := "*" + ExamPrefix + "testuser / Test Subject*\n\n"
	result := sb.String()

	if result != expected {
		t.Errorf("markupAddHeader() = %q, want %q", result, expected)
	}
}
