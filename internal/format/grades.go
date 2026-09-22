// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"strings"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// noEscape is the identity escaper. Naming it makes "escapes nothing" a stated
// choice rather than an inherited one — MarkupMsg once reached Slack's mrkdwn
// unescaped by reusing the plain-text renderer.
func noEscape(s string) string { return s }

// formatSubject writes the "<prefix><user> / <subject>" header. escape is
// mandatory: both fields are portal-derived.
func formatSubject(sb *strings.Builder, user, subject string, code msgtypes.EventCode, escape func(string) string) {
	switch code {
	case msgtypes.Exam:
		sb.WriteString(ExamPrefix)
	case msgtypes.Reading:
		sb.WriteString(ReadingPrefix)
	case msgtypes.Grade:
		sb.WriteString(GradePrefix)
	case msgtypes.FinalGrade:
		sb.WriteString(FinalGradePrefix)
	case msgtypes.NationalExam:
		sb.WriteString(NationalExamPrefix)
	default:
	}

	sb.WriteString(escape(user))
	sb.WriteString(" / ")
	sb.WriteString(escape(subject))
}

// formatGrades renders description/value pairs, skipping the blanks the scraper
// pads for alignment. escape is mandatory and covers both halves.
func formatGrades(sb *strings.Builder, descriptions, grade []string, escape func(string) string) {
	// Reslicing, rather than bounding the loop, is what makes the paired index
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
