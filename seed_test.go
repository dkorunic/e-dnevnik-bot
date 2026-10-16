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
