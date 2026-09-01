// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// TestLegacyMigrationRetriedAfterFailure pins the coupling between
// migrateLegacyQueue's return value and the legacyChecked latch.
//
// The latch exists to stop a per-queue probe transaction running on every fetch
// forever, so it must only be set once the legacy row is provably gone —
// migrated, undecodable, or absent. Latching after a *failed* migration strands
// the aggregate row permanently: it is invisible to the per-message prefix scan
// (it sits under the bare queue key, with no 0x00 separator), and the probe that
// would find it never runs again for the life of the process. Every message an
// upgrading user had queued is then silently unreachable.
//
// The failure is injected by closing the database, which makes the probe's
// transaction fail while leaving the on-disk row intact — the same shape as a
// transient lock timeout or a full disk in production.
func TestLegacyMigrationRetriedAfterFailure(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "migration-retry.db")
	queueKey := []byte("test-migration-retry-queue")

	eDB, err := sqlitedb.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	storeLegacyAggregate(t, eDB, queueKey, []msgtypes.Message{{Subject: "queued-before-upgrade"}})

	// Fail the migration: every statement errors, but the row stays on disk.
	if err := eDB.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	if got := FetchFailedMsgs(ctx, eDB, queueKey); len(got) != 0 {
		t.Fatalf("fetch against a closed database returned %+v, want nothing", got)
	}

	if _, latched := legacyChecked.Load(string(queueKey)); latched {
		t.Fatal("the legacy probe was latched after a failed migration; the aggregate row is now unreachable forever")
	}

	// Reopen the same file: the retry must find and migrate the legacy row.
	reopened, err := sqlitedb.New(ctx, dbPath)
	if err != nil {
		t.Fatalf("sqlitedb.New() reopen failed: %v", err)
	}

	defer reopened.Close() //nolint:errcheck

	got := FetchFailedMsgs(ctx, reopened, queueKey)
	if len(got) != 1 || got[0].Msg.Subject != "queued-before-upgrade" {
		t.Fatalf("FetchFailedMsgs after reopen = %+v, want the legacy message migrated on retry", got)
	}
}
