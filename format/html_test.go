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

// TestHTMLMsgEscaping verifies that HTML special characters in portal-derived
// fields are escaped so malicious grade/exam content cannot break out of the
// Telegram HTML parse mode or inject markup into email HTML alternatives.
func TestHTMLMsgEscaping(t *testing.T) {
	t.Parallel()

	// Each character that html.EscapeString rewrites must appear escaped in
	// every field that originates from the remote portal (user, subject,
	// descriptions, grades).
	username := `<script>alert("x")</script>`
	subject := `Math & "Science"`
	descriptions := []string{`<b>desc</b>`, `a&b`}
	grades := []string{`5 > 4`, `"A+"`}

	result := HTMLMsg(username, subject, msgtypes.Grade, descriptions, grades)

	// Positive assertions: raw special characters must not appear outside
	// the structural tags we emit ourselves.
	for _, forbidden := range []string{
		`<script>`,
		`alert("x")`,
		`Math & "Science"`,
		`<b>desc</b>`,
		`a&b`,
		`5 > 4`,
	} {
		if strings.Contains(result, forbidden) {
			t.Errorf("HTMLMsg leaked unescaped input %q in output: %q", forbidden, result)
		}
	}

	// Negative assertions: escaped forms must appear.
	for _, expected := range []string{
		`&lt;script&gt;`,
		`&#34;x&#34;`,
		`Math &amp; &#34;Science&#34;`,
		`&lt;b&gt;desc&lt;/b&gt;`,
		`a&amp;b`,
		`5 &gt; 4`,
	} {
		if !strings.Contains(result, expected) {
			t.Errorf("HTMLMsg output missing expected escape %q in: %q", expected, result)
		}
	}
}

// TestHtmlAddHeaderEscaping verifies the header builder escapes user/subject
// independently of HTMLMsg so header-only callers (if added later) stay safe.
func TestHtmlAddHeaderEscaping(t *testing.T) {
	t.Parallel()

	var sb strings.Builder

	htmlAddHeader(&sb, `<u>`, `A & B`, msgtypes.Exam)

	result := sb.String()
	if strings.Contains(result, `<u>`) || strings.Contains(result, `A & B`) {
		t.Errorf("htmlAddHeader leaked unescaped input: %q", result)
	}

	if !strings.Contains(result, `&lt;u&gt;`) || !strings.Contains(result, `A &amp; B`) {
		t.Errorf("htmlAddHeader missing expected escapes: %q", result)
	}
}
