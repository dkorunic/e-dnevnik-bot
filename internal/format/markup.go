// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"strings"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// MarkupMsg formats grade report as preformatted Markup block in a string.
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
// the reader as a backslash and an asterisk. Escaping the formatting
// metacharacters is therefore never right — one escaper serves header and body.
// The &/</> entities are what Slack does require, code fence included.
//
// A backtick is substituted, not escaped, for the same reason: three in a row
// would close MarkupMsg's fence and expose the rest of the note to formatting.
//
// Accepted: a literal '*' in a subject ends the header's bold early. Subject
// names and skole.hr usernames carry none, and the alternative printed a
// backslash before every underscore in every username.
var markupReplacer = strings.NewReplacer(
	`&`, `&amp;`,
	`<`, `&lt;`,
	`>`, `&gt;`,
	"`", `'`,
)

// markupEscape escapes portal content for a Slack message, header and body alike.
func markupEscape(s string) string {
	return markupReplacer.Replace(s)
}

// markupAddHeader adds Markup bold header containing username and subject name,
// and a delimiter.
func markupAddHeader(sb *strings.Builder, user, subject string, code msgtypes.EventCode) {
	sb.WriteString("*")
	formatSubject(sb, user, subject, code, markupEscape)
	sb.WriteString("*\n\n")
}
