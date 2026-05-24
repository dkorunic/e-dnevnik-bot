// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

func TestFormatMessages(t *testing.T) {
	t.Parallel()
	username := "testuser"
	subject := "testsubject"
	descriptions := []string{"desc1", "desc2"}
	grades := []string{"grade1", "grade2"}

	testCases := []struct {
		name     string
		code     msgtypes.EventCode
		formatFn func(string, string, msgtypes.EventCode, []string, []string) string
		expected string
	}{
		{
			name:     "PlainMsg Grade",
			code:     msgtypes.Grade,
			formatFn: PlainMsg,
			expected: "💯 Nova ocjena: testuser / testsubject\n\ndesc1: grade1\ndesc2: grade2\n",
		},
		{
			name:     "PlainMsg Exam",
			code:     msgtypes.Exam,
			formatFn: PlainMsg,
			expected: "⚠️ NAJAVLJEN ISPIT: testuser / testsubject\n\ndesc1: grade1\ndesc2: grade2\n",
		},
		{
			name:     "HTMLMsg Reading",
			code:     msgtypes.Reading,
			formatFn: HTMLMsg,
			expected: "<b>📚 Lektira: testuser / testsubject</b>\n<pre>\ndesc1: grade1\ndesc2: grade2\n</pre>\n",
		},
		{
			name:     "MarkupMsg FinalGrade",
			code:     msgtypes.FinalGrade,
			formatFn: MarkupMsg,
			expected: "*🎓 ZAKLJUČNA OCJENA: testuser / testsubject*\n\n```\ndesc1: grade1\ndesc2: grade2\n```\n",
		},
		{
			name:     "MarkupMsg NationalExam",
			code:     msgtypes.NationalExam,
			formatFn: MarkupMsg,
			expected: "*✍️ Nacionalni ispit: testuser / testsubject*\n\n```\ndesc1: grade1\ndesc2: grade2\n```\n",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actual := tc.formatFn(username, subject, tc.code, descriptions, grades)
			if strings.TrimSpace(actual) != strings.TrimSpace(tc.expected) {
				t.Errorf("expected:\n%s\ngot:\n%s", tc.expected, actual)
			}
		})
	}
}
