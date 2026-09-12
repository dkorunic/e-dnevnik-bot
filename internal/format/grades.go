// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import "strings"

// noEscape is the identity escaper. Naming it makes "this format escapes
// nothing" a stated choice rather than an inherited one: MarkupMsg reached
// Slack's mrkdwn unescaped by reusing the plain-text renderer.
func noEscape(s string) string { return s }

// formatGrades renders description/value pairs, skipping blank columns — the
// scraper pads those for alignment. escape covers both halves and is mandatory
// (see noEscape).
func formatGrades(sb *strings.Builder, descriptions, grade []string, escape func(string) string) {
	// Reslicing rather than bounding the loop is what makes the paired index
	// provably in range for gosec.
	n := min(len(descriptions), len(grade))
	descriptions, grade = descriptions[:n], grade[:n]

	for i, value := range grade {
		if value == "" {
			continue
		}

		sb.WriteString(escape(descriptions[i]))
		sb.WriteString(": ")
		sb.WriteString(escape(value))
		sb.WriteString("\n")
	}
}
