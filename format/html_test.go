package format

import (
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

func TestHTMLMsg(t *testing.T) {
	t.Parallel()
	username := "testuser"
	subject := "Test Subject"
	code := msgtypes.Grade
	descriptions := []string{"desc1", "desc2"}
	grade := []string{"grade1", "grade2"}

	expected := "<b>" + GradePrefix + "testuser / Test Subject</b>\n<pre>\ndesc1: grade1\ndesc2: grade2\n</pre>\n"
	result := HTMLMsg(username, subject, code, descriptions, grade)

	if result != expected {
		t.Errorf("HTMLMsg() = %q, want %q", result, expected)
	}
}

func TestHtmlAddHeader(t *testing.T) {
	t.Parallel()
	var sb strings.Builder
	user := "testuser"
	subject := "Test Subject"
	code := msgtypes.Exam

	htmlAddHeader(&sb, user, subject, code)

	expected := "<b>" + ExamPrefix + "testuser / Test Subject</b>\n"
	result := sb.String()

	if result != expected {
		t.Errorf("htmlAddHeader() = %q, want %q", result, expected)
	}
}
