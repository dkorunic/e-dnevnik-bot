// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"strings"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

// markupReplacer escapes special characters in a single pass.
// Backslash is listed first so it is replaced before any of the escape
// sequences that introduce a backslash are written. The HTML-entity triplet
// (& < >) follows because Slack mrkdwn treats <…> as link/mention delimiters
// and & as the entity-introducer; without these escapes, portal-sourced
// content containing those bytes is misparsed by Slack.
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
