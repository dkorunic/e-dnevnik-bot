// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
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

// TestLateSeedCodesSeedSilentlyOnUpgrade: absences and notes arrived after
// installs were seeded, so the first cycle after the upgrade would otherwise
// alert a whole school year of absences and every note section at once. They
// must seed per user and code, while grades keep alerting throughout, and a
// user whose scrape failed must not be marked.
// Not parallel: mutates package-level flag pointers.
func TestLateSeedCodesSeedSilentlyOnUpgrade(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	path := filepath.Join(t.TempDir(), "late.db")
	users := []string{"a@skole.hr", "b@skole.hr"}
	ok := func() *seedRun { return &seedRun{failed: &userSet{}, users: users} }

	absence := func(user, date string) msgtypes.Message {
		return msgtypes.Message{
			Code: msgtypes.Absence, Username: user, Subject: "Hrvatski jezik",
			Descriptions: []string{"Datum"}, Fields: []string{date},
		}
	}

	// The release before absences: both users seeded on grades alone, so drop
	// the per-code markers this release writes on a first run.
	prev := reopen(t, path)
	runDedupSeeded(t, prev, ok(), userGrade("a@skole.hr", "Matematika"), userGrade("b@skole.hr", "Matematika"))

	rows, err := prev.ScanPrefix(t.Context(), codeSeedKeyPrefix)
	if err != nil || len(rows) != len(users)*len(lateSeedCodes) {
		t.Fatalf("first run wrote %d per-code markers (err %v), want %d", len(rows), err, len(users)*len(lateSeedCodes))
	}

	for _, r := range rows {
		if err := prev.Delete(t.Context(), r.Key); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}

	// Upgrade cycle: B's scrape fails. A's old absence seeds silently, A's new
	// grade still alerts.
	failed := &userSet{}
	failed.add("b@skole.hr")

	got := runDedupSeeded(t, reopen(t, path), &seedRun{failed: failed, users: users},
		absence("a@skole.hr", "16.10.2026."), userGrade("a@skole.hr", "Fizika"))
	if len(got) != 1 || got[0].Code != msgtypes.Grade {
		t.Fatalf("upgrade cycle forwarded %+v, want only A's new grade — existing absences must seed silently", got)
	}

	// Next cycle: A's new absence alerts; B, unmarked after its failed scrape,
	// seeds its history now.
	got = runDedupSeeded(t, reopen(t, path), ok(),
		absence("a@skole.hr", "16.10.2026."), absence("a@skole.hr", "20.10.2026."),
		absence("b@skole.hr", "3.10.2026."))
	if len(got) != 1 || got[0].Username != "a@skole.hr" || got[0].Fields[0] != "20.10.2026." {
		t.Fatalf("second cycle forwarded %+v, want only A's new absence", got)
	}

	// And B alerts from then on.
	got = runDedupSeeded(t, reopen(t, path), ok(), absence("b@skole.hr", "3.10.2026."), absence("b@skole.hr", "5.10.2026."))
	if len(got) != 1 || got[0].Fields[0] != "5.10.2026." {
		t.Fatalf("third cycle forwarded %+v, want only B's new absence", got)
	}
}

// A first run whose /absent page was skipped saw none of the absence history:
// marking the code seeded on it would alert all of it next cycle. Notes, read
// fine, are marked; absences seed on the first cycle that reads them.
// Not parallel: mutates package-level flag pointers.
func TestSkippedCodeIsNotMarkedSeeded(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	path := filepath.Join(t.TempDir(), "skipped.db")
	users := []string{"a@skole.hr"}

	absence := func(date string) msgtypes.Message {
		return msgtypes.Message{
			Code: msgtypes.Absence, Username: "a@skole.hr", Subject: "Hrvatski jezik",
			Descriptions: []string{"Datum"}, Fields: []string{date},
		}
	}

	failed := &userSet{}
	failed.addSkipped("a@skole.hr", []msgtypes.EventCode{msgtypes.Absence})

	runDedupSeeded(t, reopen(t, path), &seedRun{failed: failed, users: users}, userGrade("a@skole.hr", "Matematika"))

	ok := func() *seedRun { return &seedRun{failed: &userSet{}, users: users} }

	if got := runDedupSeeded(t, reopen(t, path), ok(), absence("16.10.2026."), absence("17.10.2026.")); len(got) != 0 {
		t.Fatalf("first readable /absent forwarded %+v, want its history seeded silently", got)
	}

	if got := runDedupSeeded(t, reopen(t, path), ok(), absence("16.10.2026."), absence("20.10.2026.")); len(got) != 1 || got[0].Fields[0] != "20.10.2026." {
		t.Fatalf("next cycle forwarded %+v, want only the new absence", got)
	}
}
