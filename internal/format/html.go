// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"html"
	"strings"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// HTMLMsg formats grade report as preformatted HTML block in a string.
func HTMLMsg(username, subject string, code msgtypes.EventCode, descriptions, grade []string) string {
	sb := builderPool.Get().(*strings.Builder)
	defer putBuilder(sb)

	sb.Reset()
	sb.Grow(len(username) + len(subject) + 256)

	htmlAddHeader(sb, username, subject, code)

	sb.WriteString("<pre>\n")
	htmlFormatGrades(sb, descriptions, grade)
	sb.WriteString("</pre>\n")

	return sb.String()
}

// htmlAddHeader adds bold header containing username and subject name, and a delimiter.
// User and subject are HTML-escaped to prevent injection into Telegram's HTML parse mode.
func htmlAddHeader(sb *strings.Builder, user, subject string, code msgtypes.EventCode) {
	sb.WriteString("<b>")
	PlainFormatSubject(sb, html.EscapeString(user), html.EscapeString(subject), code)
	sb.WriteString("</b>\n")
}

// htmlFormatGrades formats grade descriptions and values with HTML escaping.
func htmlFormatGrades(sb *strings.Builder, descriptions, grade []string) {
	n := min(len(descriptions), len(grade))

	for i := range n {
		sb.WriteString(html.EscapeString(descriptions[i]))
		sb.WriteString(": ")
		sb.WriteString(html.EscapeString(grade[i]))
		sb.WriteString("\n")
	}
}
