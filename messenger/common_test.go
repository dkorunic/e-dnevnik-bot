// @license
// Copyright (C) 2025  Dinko Korunic
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

package messenger

import "testing"

func TestTruncateWithEllipsis(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		max   int
		want  string
	}{
		{"shorter than max", "hello", 10, "hello"},
		{"exact max length", "hello", 5, "hello"},
		{"longer than max", "hello world", 8, "hello..."},
		{"empty string", "", 5, ""},
		{"unicode multibyte", "héllo wörld", 8, "héllo..."},
		{"exactly max+1", "abcdef", 5, "ab..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateWithEllipsis(tt.input, tt.max)
			if got != tt.want {
				t.Errorf("truncateWithEllipsis(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
			}
		})
	}
}

// TestTruncateWithEllipsisRuneCount verifies Bug 12A from TESTING-PLAN:
// the result must never exceed max runes. A mutation changing `m-3` to `m-2`
// in the cutoff calculation produces a result that is m+1 runes long.
func TestTruncateWithEllipsisRuneCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		max   int
	}{
		{"ascii", "abcdefghijk", 8},
		{"unicode", "héllo wörld extra", 9},
		{"exactly-max-plus-1", "abcde", 4},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := truncateWithEllipsis(tc.input, tc.max)
			runeCount := len([]rune(got))

			if runeCount > tc.max {
				t.Errorf("truncateWithEllipsis(%q, %d) = %q has %d runes, exceeds max %d (m-3→m-2 mutation?)",
					tc.input, tc.max, got, runeCount, tc.max)
			}
		})
	}
}

// TestTruncateWithEllipsisMinimumCase verifies Bug 12B from TESTING-PLAN:
// with m=4 and a 5-rune string, the result must be exactly 4 runes
// (1 content rune + "..."). The `>=` mutation would fire one rune too early,
// producing "..." with no content rune (3 runes) for a 4-rune input.
func TestTruncateWithEllipsisMinimumCase(t *testing.T) {
	t.Parallel()

	// 5-rune ASCII string, max=4: should give "a..."
	got := truncateWithEllipsis("abcde", 4)
	runeCount := len([]rune(got))

	if runeCount != 4 {
		t.Errorf("truncateWithEllipsis(%q, 4) = %q has %d runes, want exactly 4", "abcde", got, runeCount)
	}

	if got != "a..." {
		t.Errorf("truncateWithEllipsis(%q, 4) = %q, want %q", "abcde", got, "a...")
	}
}
