// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// overflowingGrade builds a message well past Slack's cap: many rows for one
// subject, each carrying characters that render as &-entities.
func overflowingGrade() msgtypes.Message {
	const rows = 120

	d := make([]string, 0, rows)
	g := make([]string, 0, rows)

	for range rows {
		d = append(d, "Biljeska")
		g = append(g, "Odlicno znanje cijelog nastavnog sadrzaja & vise <toga>")
	}

	return msgtypes.Message{
		Code:         msgtypes.Grade,
		Username:     "pero@skole.hr",
		Subject:      "Matematika",
		Descriptions: d,
		Fields:       g,
	}
}

// TestSlackMessageTextKeepsStructureWhenTruncated: cutting the rendered string
// splits whatever it lands in, so pairs are dropped and re-rendered instead —
// the same reason truncateHTMLBody exists for the HTML formats.
func TestSlackMessageTextKeepsStructureWhenTruncated(t *testing.T) {
	t.Parallel()

	out := slackMessageText(overflowingGrade())

	if n := utf8.RuneCountInString(out); n > SlackMaxMessageChars {
		t.Errorf("rendered %d runes, over Slack's %d cap", n, SlackMaxMessageChars)
	}

	// MarkupMsg writes exactly one fenced block: an opener and a closer.
	if n := strings.Count(out, "```"); n != 2 {
		t.Errorf("found %d ``` runs, want 2 — the code block was left unterminated: %q", n, tail(out))
	}

	if !strings.HasSuffix(strings.TrimSpace(out), "```") {
		t.Errorf("output does not end with the closing fence: %q", tail(out))
	}

	// Every & must open an entity that also closes.
	for _, frag := range strings.Split(out, "&")[1:] {
		semi := strings.IndexByte(frag, ';')
		if semi < 0 || semi > 6 {
			t.Errorf("truncation split an &-entity: %q in %q", "&"+frag[:min(len(frag), 8)], tail(out))

			break
		}
	}
}

// tail returns the last 80 characters, for readable failures.
func tail(s string) string {
	if len(s) <= 80 {
		return s
	}

	return "..." + s[len(s)-80:]
}
