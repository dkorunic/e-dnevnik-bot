// @license
// Copyright (C) 2022  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package format

import (
	"strings"
)

const (
	GradePrefix   = "💯 Nova ocjena: "      // grade title prefix
	ExamPrefix    = "⚠️ NAJAVLJEN ISPIT: " // exam title prefix
	ReadingPrefix = "📚 Lektira: "
)

// PlainMsg formats grade report as cleartext block in a string.
func PlainMsg(username, subject string, isExam, isReading bool, descriptions, grade []string) string {
	sb := strings.Builder{}

	plainAddHeader(&sb, username, subject, isExam, isReading)
	plainFormatGrades(&sb, descriptions, grade)

	return sb.String()
}

// plainFormatGrades formats grade descriptions and values.
//
//nolint:interfacer
func plainFormatGrades(sb *strings.Builder, descriptions, grade []string) {
	for i := range grade {
		// grade listing will print scraped corresponding descriptions
		sb.WriteString(descriptions[i])
		sb.WriteString(": ")
		sb.WriteString(grade[i])
		sb.WriteString("\n")
	}
}

// PlainFormatSubject adds cleartext header containing prefix (event/grade), username and subject.
//
//nolint:interfacer
func PlainFormatSubject(sb *strings.Builder, user, subject string, isExam, isReading bool) {
	switch {
	case isExam:
		sb.WriteString(ExamPrefix)
	case isReading:
		sb.WriteString(ReadingPrefix)
	default:
		sb.WriteString(GradePrefix)
	}

	sb.WriteString(user)
	sb.WriteString(" / ")
	sb.WriteString(subject)
}

// plainAddHeader adds cleartext header containing username and subject name, and a delimiter.
func plainAddHeader(sb *strings.Builder, user, subject string, isExam, isReading bool) {
	PlainFormatSubject(sb, user, subject, isExam, isReading)
	sb.WriteString("\n\n")
}
