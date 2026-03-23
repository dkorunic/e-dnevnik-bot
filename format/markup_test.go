package format

import (
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

func TestMarkupMsg(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestMarkupEscapeStringBackslash verifies Bug 11 from TESTING-PLAN:
// a literal backslash in a subject name must be escaped to `\\`.
// Omitting the backslash rule from markupReplacer would let raw `\` pass through,
// breaking Slack/Telegram Markdown rendering.
func TestMarkupEscapeStringBackslash(t *testing.T) {
	t.Parallel()

	got := markupEscapeString(`\test`)
	if got != `\\test` {
		t.Errorf("markupEscapeString backslash: got %q, want %q", got, `\\test`)
	}
}

// TestMarkupEscapeStringSpecialChars verifies that all other Markdown metacharacters
// are escaped, guarding against partial removal of the replacer rules.
func TestMarkupEscapeStringSpecialChars(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		{`*bold*`, `\*bold\*`},
		{`_italic_`, `\_italic\_`},
		{"`code`", "\\`code\\`"},
		{`~strike~`, `\~strike\~`},
		{`[link]`, `\[link\]`},
		{`\back`, `\\back`},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()

			got := markupEscapeString(tc.input)
			if got != tc.want {
				t.Errorf("markupEscapeString(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
