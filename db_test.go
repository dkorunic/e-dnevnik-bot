// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// TestOpenCloseDB covers the success path of the two database lifecycle
// helpers. Their failure paths call logger.Fatal (os.Exit) and are therefore
// out of reach of an in-process test.
func TestOpenCloseDB(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "app.db.sqlite")

	eDB := openDB(t.Context(), path)
	if eDB == nil {
		t.Fatal("openDB() returned nil")
	}

	// A brand-new database must report itself as new, which is what suppresses
	// the first-run alert flood.
	if eDB.Existing() {
		t.Error("openDB() on a fresh path reported an existing database; first-run seeding would be skipped and the user flooded")
	}

	if _, err := eDB.CheckAndFlagTTL(t.Context(), "u", "s", []string{"x"}); err != nil {
		t.Fatalf("database is not usable after openDB(): %v", err)
	}

	closeDB(eDB)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("database file missing after closeDB(): %v", err)
	}
}

// TestOpenDBReopenIsExisting pins the first-run detection across restarts: a
// database that already holds rows must report Existing() on reopen, or every
// restart would silently re-seed and swallow that cycle's alerts.
func TestOpenDBReopenIsExisting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "app.db.sqlite")

	eDB := openDB(t.Context(), path)
	if _, err := eDB.CheckAndFlagTTL(t.Context(), "u", "s", []string{"seed"}); err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}

	closeDB(eDB)

	eDB = openDB(t.Context(), path)
	defer closeDB(eDB)

	if !eDB.Existing() {
		t.Error("a reopened, populated database reported itself as new; every restart would suppress that cycle's alerts")
	}

	// The flag written before the restart must still suppress a duplicate.
	found, err := eDB.CheckAndFlagTTL(t.Context(), "u", "s", []string{"seed"})
	if err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}

	if !found {
		t.Error("a hash flagged before the restart was not found afterwards; alerts would repeat every run")
	}
}

// TestOpenDBDedupSurvivesAcrossHelpers is an end-to-end check of the dedup
// contract the helpers exist to serve: distinct events are independent, and an
// identical event is suppressed on the second sighting.
func TestOpenDBDedupSurvivesAcrossHelpers(t *testing.T) {
	t.Parallel()

	eDB := openDB(t.Context(), filepath.Join(t.TempDir(), "dedup.db.sqlite"))
	defer closeDB(eDB)

	msgs := []msgtypes.Message{
		{Code: msgtypes.Grade, Username: "u", Subject: "Matematika", Fields: []string{"1.9.", "5"}},
		{Code: msgtypes.Grade, Username: "u", Subject: "Matematika", Fields: []string{"1.9.", "4"}},
		{Code: msgtypes.Grade, Username: "u", Subject: "Fizika", Fields: []string{"1.9.", "5"}},
	}

	for i, m := range msgs {
		found, err := eDB.CheckAndFlagTTL(t.Context(), m.Username, m.Subject, m.Fields)
		if err != nil {
			t.Fatalf("message %d: CheckAndFlagTTL() failed: %v", i, err)
		}

		if found {
			t.Errorf("message %d was reported as already seen; distinct events must not collide", i)
		}
	}

	found, err := eDB.CheckAndFlagTTL(t.Context(), msgs[0].Username, msgs[0].Subject, msgs[0].Fields)
	if err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}

	if !found {
		t.Error("a repeated event was not detected as a duplicate; the alert would fire twice")
	}
}
