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

// TestLegacyMigrationRetriedAfterFailure: legacyChecked may only latch once the
// legacy row is provably gone. Latching after a failed migration strands it for
// the process lifetime — the row sits under the bare queue key, so the
// per-message prefix scan never sees it and the probe never runs again, leaving
// an upgrading user's whole queue unreachable.
//
// Closing the database fails the probe while leaving the row on disk, matching a
// lock timeout or a full disk.
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
