// @license
// Copyright (C) 2025 Dinko Korunic
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
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT "AS IS", WITHOUT WARRANTY OF ANY, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package format

import (
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

func TestFormatMessages(t *testing.T) {
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
			actual := tc.formatFn(username, subject, tc.code, descriptions, grades)
			if strings.TrimSpace(actual) != strings.TrimSpace(tc.expected) {
				t.Errorf("expected:\n%s\ngot:\n%s", tc.expected, actual)
			}
		})
	}
}
