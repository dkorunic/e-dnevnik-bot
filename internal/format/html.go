// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"html"
	"strings"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// HTMLMsg renders the report as a preformatted HTML block.
func HTMLMsg(username, subject string, code msgtypes.EventCode, descriptions, grade []string) string {
	var sb strings.Builder

	sb.Grow(len(username) + len(subject) + 256)

	htmlAddHeader(&sb, username, subject, code)

	sb.WriteString("<pre>\n")
	formatGrades(&sb, descriptions, grade, html.EscapeString)
	sb.WriteString("</pre>\n")

	return sb.String()
}

// htmlAddHeader writes the bold header. Escaped to keep portal content out of
// Telegram's HTML parse mode.
func htmlAddHeader(sb *strings.Builder, user, subject string, code msgtypes.EventCode) {
	sb.WriteString("<b>")
	formatSubject(sb, user, subject, code, html.EscapeString)
	sb.WriteString("</b>\n")
}
