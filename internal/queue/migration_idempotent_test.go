// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// A failed delete leaves the aggregate behind, and the in-process latch dies
// with the process, so the next run migrates it again. That must overwrite the
// first copy, not add a second one: every queued alert would arrive twice.
func TestLegacyMigrationRepeatedAfterRestartDoesNotDuplicate(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "idempotent.db.sqlite")
	queueKey := []byte("test-idempotent-migration-queue")

	eDB, err := sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	storeLegacyAggregate(t, eDB, queueKey, []msgtypes.Message{{Subject: "one"}, {Subject: "two"}})

	admin, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open() = %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if _, err := admin.Exec(`CREATE TRIGGER no_delete BEFORE DELETE ON kv BEGIN SELECT RAISE(ABORT, 'busy'); END`); err != nil {
		t.Fatalf("CREATE TRIGGER = %v", err)
	}

	if got := FetchFailedMsgs(t.Context(), eDB, queueKey); len(got) != 2 {
		t.Fatalf("first fetch = %+v, want both messages", got)
	}

	if _, err := admin.Exec(`DROP TRIGGER no_delete`); err != nil {
		t.Fatalf("DROP TRIGGER = %v", err)
	}

	// What a restart does to the latch.
	legacyChecked.Delete(string(queueKey))

	if got := FetchFailedMsgs(t.Context(), eDB, queueKey); len(got) != 2 {
		t.Errorf("fetch after restart = %d messages, want 2: %+v", len(got), got)
	}
}
