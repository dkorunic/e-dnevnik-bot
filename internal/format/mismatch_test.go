// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// TestFormattersTolerateMismatchedFieldCounts guards the min() bound every
// formatter shares. Descriptions come from a header row and values from data
// rows, so portal HTML drift makes them different lengths; indexing the longer
// one panics. Scrapers run with no panic guard, so that takes down the process
// rather than degrading one messenger.
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
