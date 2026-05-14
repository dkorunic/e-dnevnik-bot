// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"strings"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

// markupReplacer escapes Markdown specials in one pass.
// Backslash is first so later escape sequences don't introduce one mid-pass.
// HTML-entity triplet escapes Slack mrkdwn link/mention metasyntax.
var markupReplacer = strings.NewReplacer(
	`\`, `\\`,
	`&`, `&amp;`,
	`<`, `&lt;`,
	`>`, `&gt;`,
	`*`, `\*`,
	`_`, `\_`,
	"`", "\\`",
	`~`, `\~`,
	`[`, `\[`,
	`]`, `\]`,
)

// MarkupMsg formats grade report as preformatted Markup block in a string.
func MarkupMsg(username, subject string, code msgtypes.EventCode, descriptions, grade []string) string {
	sb := builderPool.Get().(*strings.Builder)
	defer putBuilder(sb)

	sb.Reset()
	sb.Grow(len(username) + len(subject) + 256)

	markupAddHeader(sb, username, subject, code)

	sb.WriteString("```\n")
	plainFormatGrades(sb, descriptions, grade)
	sb.WriteString("```\n")

	return sb.String()
}

// markupEscapeString escapes Markdown special characters in s to prevent them
// from being interpreted as formatting syntax in Slack mrkdwn and Telegram
// MarkdownV1 (e.g. a subject name containing '*' or '_' would break bold wrapping).
func markupEscapeString(s string) string {
	return markupReplacer.Replace(s)
}

// markupAddHeader adds Markup bold header containing username and subject name, and a delimiter.
// User and subject are escaped to prevent Markdown metacharacters from breaking the bold syntax.
func markupAddHeader(sb *strings.Builder, user, subject string, code msgtypes.EventCode) {
	sb.WriteString("*")
	PlainFormatSubject(sb, markupEscapeString(user), markupEscapeString(subject), code)
	sb.WriteString("*\n\n")
}
