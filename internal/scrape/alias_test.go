// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"strings"
	"testing"
)

// TestTrimAllSpaceDoesNotAliasPriorResults: results become Message.Fields — the
// dedup identity — and String() aliases the builder's buffer, so builder reuse
// would silently corrupt a hash.
func TestTrimAllSpaceDoesNotAliasPriorResults(t *testing.T) {
	t.Parallel()

	first := trimAllSpace("  jedan   dva  ")

	// Clone, not assign: a plain copy shares the backing array.
	want := strings.Clone(first)

	for i := range 32 {
		pad := strings.Repeat("X", 64+i)
		_ = trimAllSpace("  " + pad + "   " + pad + "  ")
	}

	if first != want {
		t.Errorf("a later call mutated an earlier result: got %q, want %q", first, want)
	}
}

func BenchmarkTrimAllSpace(b *testing.B) {
	// Hits the rewrite path: leading, trailing and repeated whitespace, as the
	// <pre> note cells arrive.
	const s = "  Odlično  znanje\n\n  cijelog   nastavnog sadržaja  \t "

	b.ReportAllocs()

	for b.Loop() {
		_ = trimAllSpace(s)
	}
}
