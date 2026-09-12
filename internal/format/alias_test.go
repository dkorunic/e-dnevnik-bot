// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package format

import (
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// TestFormattersDoNotAliasPriorResults pins the invariant that rules out
// builder reuse: String() aliases the builder's buffer, and the messengers hold
// formatted bodies across retries and queue writes.
func TestFormattersDoNotAliasPriorResults(t *testing.T) {
	t.Parallel()

	formatters := map[string]func(string, string, msgtypes.EventCode, []string, []string) string{
		"PlainMsg":  PlainMsg,
		"HTMLMsg":   HTMLMsg,
		"MarkupMsg": MarkupMsg,
	}

	for name, format := range formatters {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			first := format("user-one", "subject-one", msgtypes.Grade, []string{"desc"}, []string{"5"})

			// Clone, not assign: a shared copy would be corrupted identically
			// and the comparison would pass.
			want := strings.Clone(first)

			// Longer each time, to overwrite well past the first result.
			for i := range 32 {
				pad := strings.Repeat("X", 64+i)
				_ = format(pad, pad, msgtypes.Grade, []string{pad}, []string{pad})
			}

			if first != want {
				t.Errorf("a later call mutated an earlier result: got %q, want %q", first, want)
			}
		})
	}
}

func BenchmarkPlainMsg(b *testing.B) {
	descriptions := []string{"Datum", "Ocjena", "Bilješka"}
	grade := []string{"15.4.", "5", "Odlično znanje cijelog nastavnog sadržaja"}

	b.ReportAllocs()

	for b.Loop() {
		_ = PlainMsg("pero.peric@skole.hr", "Matematika", msgtypes.Grade, descriptions, grade)
	}
}

func BenchmarkHTMLMsg(b *testing.B) {
	descriptions := []string{"Datum", "Ocjena", "Bilješka"}
	grade := []string{"15.4.", "5", "Odlično znanje cijelog nastavnog sadržaja"}

	b.ReportAllocs()

	for b.Loop() {
		_ = HTMLMsg("pero.peric@skole.hr", "Matematika", msgtypes.Grade, descriptions, grade)
	}
}
