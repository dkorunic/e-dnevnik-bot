// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"strings"
	"sync"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

var builderPool = sync.Pool{
	New: func() any { return new(strings.Builder) },
}

const (
	GradePrefix        = "💯 Nova ocjena: "
	ExamPrefix         = "⚠️ NAJAVLJEN ISPIT: "
	ReadingPrefix      = "📚 Lektira: "
	FinalGradePrefix   = "🎓 ZAKLJUČNA OCJENA: "
	NationalExamPrefix = "✍️ Nacionalni ispit: "

	// maxPooledBuilderCap caps the capacity of pooled builders so a single very
	// large message does not permanently inflate every entry in the pool.
	maxPooledBuilderCap = 64 * 1024
)

// putBuilder returns sb to the pool after Reset. Builders whose backing buffer
// has grown beyond maxPooledBuilderCap are dropped on the floor so that one
// outlier message cannot bloat the pool indefinitely. Calling sb.String()
// before Reset is safe — strings.Builder.String shares the underlying byte
// slice via unsafe.String, so the returned string keeps the data alive even
// after Reset clears the builder.
func putBuilder(sb *strings.Builder) {
	if sb.Cap() > maxPooledBuilderCap {
		return
	}

	sb.Reset()
	builderPool.Put(sb)
}

// PlainMsg formats grade report as cleartext block in a string.
func PlainMsg(username, subject string, code msgtypes.EventCode, descriptions, grade []string) string {
	sb := builderPool.Get().(*strings.Builder)
	defer putBuilder(sb)

	sb.Reset()
	sb.Grow(len(username) + len(subject) + 256)

	plainAddHeader(sb, username, subject, code)
	plainFormatGrades(sb, descriptions, grade)

	return sb.String()
}

// plainFormatGrades formats grade descriptions and values.
//
//nolint:interfacer
func plainFormatGrades(sb *strings.Builder, descriptions, grade []string) {
	n := min(len(descriptions), len(grade))

	for i := range n {
		sb.WriteString(descriptions[i])
		sb.WriteString(": ")
		sb.WriteString(grade[i])
		sb.WriteString("\n")
	}
}

// PlainFormatSubject adds cleartext header containing prefix (event/grade), username and subject.
//
//nolint:interfacer
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
