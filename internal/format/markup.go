// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"strings"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// MarkupMsg renders the report as a fenced Slack mrkdwn block.
func MarkupMsg(username, subject string, code msgtypes.EventCode, descriptions, grade []string) string {
	var sb strings.Builder

	sb.Grow(len(username) + len(subject) + 256)

	markupAddHeader(&sb, username, subject, code)

	sb.WriteString("```\n")
	formatGrades(&sb, descriptions, grade, markupEscape)
	sb.WriteString("```\n")

	return sb.String()
}

// markupReplacer escapes what Slack interprets, and nothing else.
//
// Confirmed on a live render: mrkdwn honours no backslash escape, so `\*` reaches
// the reader as a backslash and an asterisk. Escaping metacharacters is
// therefore never right, and one escaper serves header and body alike. The
// &/</> entities are what Slack does require, inside the fence included.
//
// A backtick is substituted rather than escaped for the same reason: three in a
// row would close MarkupMsg's fence.
//
// Accepted: a literal '*' in a subject ends the header's bold early. Subjects
// and skole.hr usernames carry none, and the alternative printed a backslash
// before every underscore in every username.
var markupReplacer = strings.NewReplacer(
	`&`, `&amp;`,
	`<`, `&lt;`,
	`>`, `&gt;`,
	"`", `'`,
)

// markupEscape escapes portal content, header and body alike.
func markupEscape(s string) string {
	return markupReplacer.Replace(s)
}

// markupAddHeader writes the bold header.
func markupAddHeader(sb *strings.Builder, user, subject string, code msgtypes.EventCode) {
	sb.WriteString("*")
	formatSubject(sb, user, subject, code, markupEscape)
	sb.WriteString("*\n\n")
}
