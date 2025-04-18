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

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

// HTMLMsg formats grade report as preformatted HTML block in a string.
func HTMLMsg(username, subject string, code msgtypes.EventCode, descriptions, grade []string) string {
	sb := strings.Builder{}

	htmlAddHeader(&sb, username, subject, code)

	sb.WriteString("<pre>\n")
	plainFormatGrades(&sb, descriptions, grade)
	sb.WriteString("</pre>\n")

	return sb.String()
}

// htmlAddHeader adds bold header containing username and subject name, and a delimiter.
func htmlAddHeader(sb *strings.Builder, user, subject string, code msgtypes.EventCode) {
	sb.WriteString("<b>")
	PlainFormatSubject(sb, user, subject, code)
	sb.WriteString("</b>\n")
}
