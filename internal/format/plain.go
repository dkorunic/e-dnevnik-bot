// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"strings"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

const (
	GradePrefix        = "💯 Nova ocjena: "
	ExamPrefix         = "⚠️ NAJAVLJEN ISPIT: "
	ReadingPrefix      = "📚 Lektira: "
	FinalGradePrefix   = "🎓 ZAKLJUČNA OCJENA: "
	NationalExamPrefix = "✍️ Nacionalni ispit: "
)

// Formatters build into a local builder, never a pooled one: String() aliases
// the builder's buffer, so reuse would overwrite results callers still hold.

// PlainMsg formats grade report as cleartext block in a string.
func PlainMsg(username, subject string, code msgtypes.EventCode, descriptions, grade []string) string {
	var sb strings.Builder

	sb.Grow(len(username) + len(subject) + 256)

	plainAddHeader(&sb, username, subject, code)
	formatGrades(&sb, descriptions, grade, noEscape)

	return sb.String()
}

// PlainSubject returns the prefix + user + " / " + subject header as a string.
func PlainSubject(user, subject string, code msgtypes.EventCode) string {
	var sb strings.Builder

	sb.Grow(len(user) + len(subject) + 40)

	PlainFormatSubject(&sb, user, subject, code)

	return sb.String()
}

// PlainFormatSubject adds cleartext header containing prefix (event/grade), username and subject.
func PlainFormatSubject(sb *strings.Builder, user, subject string, code msgtypes.EventCode) {
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

	sb.WriteString(user)
	sb.WriteString(" / ")
	sb.WriteString(subject)
}

// plainAddHeader adds cleartext header containing username and subject name, and a delimiter.
func plainAddHeader(sb *strings.Builder, user, subject string, code msgtypes.EventCode) {
	PlainFormatSubject(sb, user, subject, code)
	sb.WriteString("\n\n")
}
