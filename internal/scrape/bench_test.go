// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// gradesPage builds a /grade/all page of subjects × rows grades, padded with
// head and nav chrome so the parser pays for a portal-sized document, not just
// the tables.
func gradesPage(subjects, rows int) []byte {
	var b strings.Builder

	b.WriteString(`<html><head>`)
	b.WriteString(strings.Repeat(`<script>var x=1;</script><link rel="stylesheet" href="/a.css">`, 40))
	b.WriteString(`</head><body><div id="page-wrapper"><nav>`)
	b.WriteString(strings.Repeat(`<a href="/x">menu</a>`, 60))
	b.WriteString(`</nav><span class="logged-in-user">u</span><div class="content">`)

	for s := range subjects {
		fmt.Fprintf(&b, `<div class="flex-table new-grades-table" data-action-id="Predmet %d">`+
			`<div class="row header first"><div class="cell">Predmet %d</div></div>`+
			`<div class="row header"><div class="cell"><span>Datum</span></div><div class="cell"><span>Element vrednovanja</span></div>`+
			`<div class="cell"><span>Ocjena</span></div><div class="cell"><span>Bilješka</span></div></div>`, s, s)

		for r := range rows {
			// The <pre> note arrives with newlines, so trimAllSpace rewrites it.
			fmt.Fprintf(&b, `<div class="row"><div class="cell"><span>%d.%d.</span></div>`+
				`<div class="cell"><span>Usmeno   ispitivanje</span></div><div class="cell"><span>%d</span></div>`+
				"<div class=\"cell\"><pre>Bilješka\n  nastavnika %d</pre></div></div>", r%28+1, r%12+1, r%5+1, r)
		}

		b.WriteString(`</div>`)
	}

	b.WriteString(`</div></div></body></html>`)

	return []byte(b.String())
}

// BenchmarkParseGradesPage parses a 15-subject, 300-grade page: about one
// class's grades per cycle.
func BenchmarkParseGradesPage(b *testing.B) {
	const subjects, rows = 15, 20

	page := gradesPage(subjects, rows)
	ctx := context.Background()

	// Allocated once and drained, so the channel is not charged to the parser.
	ch := make(chan msgtypes.Message, subjects*rows)

	b.SetBytes(int64(len(page)))
	b.ReportAllocs()

	for b.Loop() {
		if err := parseGrades(ctx, ch, "u", page, false, "1.a"); err != nil {
			b.Fatal(err)
		}

		if len(ch) != subjects*rows {
			b.Fatalf("parsed %d grades, want %d", len(ch), subjects*rows)
		}

		for range subjects * rows {
			<-ch
		}
	}
}
