// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"html"
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// TestFormatGrades covers the body renderer every format shares. The escaper is
// a required argument because MarkupMsg once inherited the unescaped
// plain-text path.
func TestFormatGrades(t *testing.T) {
	t.Parallel()

	tests := []struct {
		escape       func(string) string
		name         string
		want         string
		descriptions []string
		grade        []string
	}{
		{
			name:         "escaper applies to description and value alike",
			escape:       html.EscapeString,
			descriptions: []string{"<desc>"},
			grade:        []string{"<value>"},
			want:         "&lt;desc&gt;: &lt;value&gt;\n",
		},
		{
			name:         "noEscape passes content through verbatim",
			escape:       noEscape,
			descriptions: []string{"<desc>"},
			grade:        []string{"<value>"},
			want:         "<desc>: <value>\n",
		},
		{
			name:         "blank values are alignment padding, not content",
			escape:       noEscape,
			descriptions: []string{"Datum", "Ocjena", "Bilješka"},
			grade:        []string{"15.4.", "", "5"},
			want:         "Datum: 15.4.\nBilješka: 5\n",
		},
		{
			name:         "surplus descriptions are dropped, not mispaired",
			escape:       noEscape,
			descriptions: []string{"Datum", "Ocjena", "Bilješka"},
			grade:        []string{"15.4."},
			want:         "Datum: 15.4.\n",
		},
		{
			name:         "surplus values are dropped, not mispaired",
			escape:       noEscape,
			descriptions: []string{"Datum"},
			grade:        []string{"15.4.", "5"},
			want:         "Datum: 15.4.\n",
		},
		{
			name:   "no pairs renders nothing",
			escape: noEscape,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var sb strings.Builder

			formatGrades(&sb, tt.descriptions, tt.grade, tt.escape)

			if got := sb.String(); got != tt.want {
				t.Errorf("formatGrades() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestFormatSubject covers the shared header line. The escaper is required so
// the three *AddHeader wrappers cannot each reach their own answer.
func TestFormatSubject(t *testing.T) {
	t.Parallel()

	tests := []struct {
		escape func(string) string
		name   string
		want   string
	}{
		{
			name:   "escaper covers user and subject alike",
			escape: html.EscapeString,
			want:   GradePrefix + "&lt;u&gt; / &lt;s&gt;",
		},
		{
			name:   "noEscape passes both through verbatim",
			escape: noEscape,
			want:   GradePrefix + "<u> / <s>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var sb strings.Builder

			formatSubject(&sb, "<u>", "<s>", msgtypes.Grade, tt.escape)

			if got := sb.String(); got != tt.want {
				t.Errorf("formatSubject() = %q, want %q", got, tt.want)
			}
		})
	}
}
