// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// TestFormattersTolerateMismatchedFieldCounts guards the min() bound shared by
// every formatter. Descriptions come from a table's header row and values from
// its data rows — they are scraped separately, so any portal HTML drift (an
// extra header cell, a missing data cell) makes the two slices different
// lengths. Indexing on the longer one panics.
//
// The blast radius is what makes this worth pinning: scrapers run with no panic
// guard of their own, so a panic here takes down the whole process rather than
// degrading one messenger. A formatter must render the pairs it has and drop
// the rest.
func TestFormattersTolerateMismatchedFieldCounts(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		descriptions []string
		grade        []string
	}{
		{name: "more descriptions than grades", descriptions: []string{"a", "b", "c"}, grade: []string{"1"}},
		{name: "more grades than descriptions", descriptions: []string{"a"}, grade: []string{"1", "2", "3"}},
		{name: "descriptions only", descriptions: []string{"a", "b"}, grade: nil},
		{name: "grades only", descriptions: nil, grade: []string{"1", "2"}},
		{name: "both empty", descriptions: nil, grade: nil},
	}

	formatters := map[string]func(string, string, msgtypes.EventCode, []string, []string) string{
		"PlainMsg":  PlainMsg,
		"HTMLMsg":   HTMLMsg,
		"MarkupMsg": MarkupMsg,
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for name, fn := range formatters {
				got := fn("pero.peric", "Matematika", msgtypes.Grade, tt.descriptions, tt.grade)
				if got == "" {
					t.Errorf("%s() returned an empty string; the header must always be present", name)
				}
			}
		})
	}
}
