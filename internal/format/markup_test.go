// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
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

// TestMarkupEscape pins what Slack actually interprets. Confirmed against a
// live render: mrkdwn honours no backslash escape, so escaping the formatting
// metacharacters printed the backslashes verbatim — a `\_` in every username
// carrying an underscore. Only the &/</> entities are required, and a backtick
// has to be substituted because it cannot be escaped either.
func TestMarkupEscape(t *testing.T) {
	t.Parallel()

	cases := []struct {
		input string
		want  string
	}{
		// Slack requires these, everywhere.
		{`&`, `&amp;`},
		{`<`, `&lt;`},
		{`>`, `&gt;`},
		{`<a&b>`, `&lt;a&amp;b&gt;`},
		{`<http://evil.example|here>`, `&lt;http://evil.example|here&gt;`},

		// Formatting metacharacters survive as typed: escaping them would print
		// a backslash, and Slack would still format them.
		{`*bold*`, `*bold*`},
		{`_italic_`, `_italic_`},
		{`~strike~`, `~strike~`},
		{`[link]`, `[link]`},
		{`pero_peric@skole.hr`, `pero_peric@skole.hr`},

		// A backslash is not special to Slack, so it is left alone rather than
		// doubled.
		{`\back`, `\back`},

		// Substituted, not escaped: three of them would close MarkupMsg's fence.
		{"`code`", "'code'"},
		{"```", "'''"},

		// NewReplacer is single-pass — no double-escape.
		{`a & <b> *c*`, `a &amp; &lt;b&gt; *c*`},
	}

	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			t.Parallel()

			got := markupEscape(tc.input)
			if got != tc.want {
				t.Errorf("markupEscape(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestMarkupMsgEscapesBody: Slack's mrkdwn reads <...|...> as a link and ``` as
// a fence terminator even inside a code block, so portal-derived body content
// needs escaping, not just the header.
func TestMarkupMsgEscapesBody(t *testing.T) {
	t.Parallel()

	result := MarkupMsg("pero@skole.hr", "Matematika", msgtypes.Grade,
		[]string{`<b>Bilješka</b>`},
		[]string{`see <http://evil.example|here> & *bold*`})

	// Only what Slack actually interprets: the entity trio. Formatting
	// metacharacters are inert inside the fence and must survive as typed.
	for _, forbidden := range []string{
		`<b>Bilješka</b>`,
		`<http://evil.example|here>`,
	} {
		if strings.Contains(result, forbidden) {
			t.Errorf("MarkupMsg leaked unescaped input %q in output: %q", forbidden, result)
		}
	}

	for _, expected := range []string{
		`&lt;b&gt;Bilješka&lt;/b&gt;`,
		`&lt;http://evil.example|here&gt;`,
		`&amp;`,
	} {
		if !strings.Contains(result, expected) {
			t.Errorf("MarkupMsg output missing escaped form %q: %q", expected, result)
		}
	}

	// mrkdwn applies no formatting inside a code fence and has no backslash
	// escape, so a backslash added here reaches the reader as a backslash.
	if strings.Contains(result, `\`) {
		t.Errorf("MarkupMsg put a literal backslash in the fenced body: %q", result)
	}

	// A note keeps the characters the teacher typed.
	if !strings.Contains(result, `*bold*`) {
		t.Errorf("MarkupMsg altered inert metacharacters in the body: %q", result)
	}
}

// TestMarkupMsgBodyKeepsFenceIntact: a backtick cannot be escaped in mrkdwn, so
// three of them in a note would close the block early and expose the remainder
// to formatting. Substitution is the only way to keep the fence whole without
// printing a backslash.
func TestMarkupMsgBodyKeepsFenceIntact(t *testing.T) {
	t.Parallel()

	result := MarkupMsg("u", "Matematika", msgtypes.Grade,
		[]string{"Bilješka"},
		[]string{"pazi ``` ovdje"})

	// Exactly the two fences MarkupMsg writes itself.
	if got := strings.Count(result, "```"); got != 2 {
		t.Errorf("found %d ``` runs, want the 2 MarkupMsg emits: %q", got, result)
	}

	if strings.Contains(result, `\`) {
		t.Errorf("backtick was escaped rather than substituted: %q", result)
	}
}

// TestMarkupAddHeaderEscaping is the header counterpart to
// TestMarkupMsgEscapesBody. Both call sites pass the same escaper: Slack honours
// no backslash escape anywhere, so there is only one correct answer for this
// renderer and the header must not have its own.
func TestMarkupAddHeaderEscaping(t *testing.T) {
	t.Parallel()

	var sb strings.Builder

	markupAddHeader(&sb, "pero_peric@skole.hr", "<b>Matematika *i* fizika</b>", msgtypes.Grade)

	result := sb.String()

	// Confirmed against a live Slack render: a backslash has no escaping effect
	// there, it just reaches the reader as a backslash. The header is as much
	// portal content as the body.
	if strings.Contains(result, `\`) {
		t.Errorf("markupAddHeader put a literal backslash in the header: %q", result)
	}

	// The entity trio is what Slack does require, everywhere.
	if !strings.Contains(result, `&lt;b&gt;`) {
		t.Errorf("markupAddHeader did not entity-escape the header: %q", result)
	}
}
