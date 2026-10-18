// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"
)

// TestLoadSeededRetriesMigrationAfterFailedMarker: a marker write that fails
// during the pre-marker migration must not be followed by the migration key,
// or that user reads as a first run next cycle and its new events are flagged
// without alerting. A trigger fails the write for real, so no seam is needed.
// Not parallel: mutates package-level flag pointers.
func TestLoadSeededRetriesMigrationAfterFailedMarker(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	path := filepath.Join(t.TempDir(), "legacy.db")
	users := []string{"a@skole.hr", "b@skole.hr"}

	// Pre-marker history for both users.
	runDedup(t, t.Context(), reopen(t, path),
		userGrade("a@skole.hr", "Matematika"), userGrade("b@skole.hr", "Matematika"))

	raw, err := sql.Open("sqlite", path+".sqlite")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	t.Cleanup(func() { _ = raw.Close() })

	bKey := hex.EncodeToString(append(append([]byte{}, seedKeyPrefix...), "b@skole.hr"...))
	if _, err = raw.ExecContext(t.Context(), "CREATE TRIGGER fail_b BEFORE INSERT ON kv WHEN NEW.key = X'"+bKey+
		"' BEGIN SELECT RAISE(ABORT, 'injected'); END"); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	runDedupSeeded(t, reopen(t, path), &seedRun{failed: &userSet{}, users: users})

	if _, err = raw.ExecContext(t.Context(), "DROP TRIGGER fail_b"); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	got := runDedupSeeded(t, reopen(t, path), &seedRun{failed: &userSet{}, users: users},
		userGrade("b@skole.hr", "Fizika"))
	if len(got) != 1 || got[0].Subject != "Fizika" {
		t.Fatalf("cycle after a failed marker write forwarded %+v, want b's new Fizika grade", got)
	}
}

// TestLoadSeededFreshInstallWithoutMigrationKeyFlagsNothing: on a fresh
// database the migration key is what says "markers are in use". If it cannot
// be written while A's events are flagged, the next cycle sees rows without the
// key, takes the database for a pre-marker one and marks every user seeded — so
// B, whose first scrape failed, alerted its entire history. Such a cycle must
// flag nothing. Not parallel: mutates package-level flag pointers.
func TestLoadSeededFreshInstallWithoutMigrationKeyFlagsNothing(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	path := filepath.Join(t.TempDir(), "fresh.db")
	users := []string{"a@skole.hr", "b@skole.hr"}

	cycle1 := reopen(t, path)

	raw, err := sql.Open("sqlite", path+".sqlite")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}

	t.Cleanup(func() { _ = raw.Close() })

	keyHex := hex.EncodeToString(seedMigratedKey)
	if _, err = raw.ExecContext(t.Context(), "CREATE TRIGGER fail_key BEFORE INSERT ON kv WHEN NEW.key = X'"+keyHex+
		"' BEGIN SELECT RAISE(ABORT, 'injected'); END"); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	// Cycle 1, fresh install: A scrapes, B's scrape fails.
	failed := &userSet{}
	failed.add("b@skole.hr")

	runDedupSeeded(t, cycle1, &seedRun{failed: failed, users: users}, userGrade("a@skole.hr", "Matematika"))

	if _, err = raw.ExecContext(t.Context(), "DROP TRIGGER fail_key"); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	// Cycle 2: both scrape their history. Both are still first runs.
	got := runDedupSeeded(t, reopen(t, path), &seedRun{failed: &userSet{}, users: users},
		userGrade("a@skole.hr", "Matematika"), userGrade("b@skole.hr", "Fizika"))
	if len(got) != 0 {
		t.Fatalf("cycle after an unrecorded fresh-install migration forwarded %+v, want nothing", got)
	}
}
