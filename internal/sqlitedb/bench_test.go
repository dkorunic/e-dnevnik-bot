// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package sqlitedb

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
)

const benchSeeded = 2000

// benchDB returns a database already holding benchSeeded flagged events.
func benchDB(b *testing.B) *Edb {
	b.Helper()

	ctx := context.Background()

	db, err := New(ctx, filepath.Join(b.TempDir(), "bench"))
	if err != nil {
		b.Fatal(err)
	}

	b.Cleanup(func() { _ = db.Close() })

	for i := range benchSeeded {
		if _, err := db.CheckAndFlagTTL(ctx, "user", "Predmet", []string{strconv.Itoa(i), "5"}); err != nil {
			b.Fatal(err)
		}
	}

	return db
}

// BenchmarkCheckAndFlagTTLHit is the steady state: every cycle re-scrapes
// events already flagged.
func BenchmarkCheckAndFlagTTLHit(b *testing.B) {
	db := benchDB(b)
	ctx := context.Background()

	b.ReportAllocs()

	i := 0
	for b.Loop() {
		found, err := db.CheckAndFlagTTL(ctx, "user", "Predmet", []string{strconv.Itoa(i % benchSeeded), "5"})
		if err != nil || !found {
			b.Fatalf("found=%v err=%v", found, err)
		}

		i++
	}
}

// BenchmarkCheckAndFlagTTLMiss is a new event: the legacy-key probe plus the
// insert.
func BenchmarkCheckAndFlagTTLMiss(b *testing.B) {
	db := benchDB(b)
	ctx := context.Background()

	b.ReportAllocs()

	i := 0
	for b.Loop() {
		found, err := db.CheckAndFlagTTL(ctx, "user", "Novo", []string{strconv.Itoa(i), "5"})
		if err != nil || found {
			b.Fatalf("found=%v err=%v", found, err)
		}

		i++
	}
}

func BenchmarkHashContent(b *testing.B) {
	target := []string{"12.3.", "Usmeno ispitivanje", "5", "Bilješka nastavnika"}

	b.ReportAllocs()

	for b.Loop() {
		_ = hashContent("user@skole.hr", "Matematika", target)
	}
}
