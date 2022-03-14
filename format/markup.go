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

// MarkupMsg formats grade report as preformatted Markup block in a string.
func MarkupMsg(username, subject string, descriptions, grade []string) string {
	sb := &strings.Builder{}

	markupAddHeader(sb, username, subject)

	sb.WriteString("```\n")
	// grade listing will print scraped corresponding descriptions
	for i := range grade {
		sb.WriteString(descriptions[i])
		sb.WriteString(": ")
		sb.WriteString(grade[i])
		sb.WriteString("\n")
	}
	sb.WriteString("```\n")

	return sb.String()
}

// markupAddHeader adds Markup bold header containing username and subject name.
func markupAddHeader(sb *strings.Builder, user, subject string) {
	sb.WriteString("*")
	sb.WriteString(user)
	sb.WriteString(" / ")
	sb.WriteString(subject)
	sb.WriteString("*\n\n")
}
