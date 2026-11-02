// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"path/filepath"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// A new database handle opens every poll cycle, so a count nobody forgets pins a
// closed handle for each failing cycle of a daemon's life.
func TestForgetStoreFailuresDropsOnlyThatDatabase(t *testing.T) {
	t.Parallel()

	failingDB := func(name string) *sqlitedb.Edb {
		eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatalf("sqlitedb.New() = %v", err)
		}

		_ = eDB.Close()

		if err := StoreFailedMsgs(t.Context(), eDB, []byte("q"), msgtypes.Message{}); err == nil {
			t.Fatal("StoreFailedMsgs() on a closed database succeeded")
		}

		return eDB
	}

	finished, running := failingDB("finished"), failingDB("running")

	ForgetStoreFailures(finished)

	if got := StoreFailureCount(finished, []byte("q")); got != 0 {
		t.Errorf("StoreFailureCount(finished) = %d, want 0 once forgotten", got)
	}

	if !anyStoreFailure(running) {
		t.Error("forgetting one database also forgot another's failures")
	}
}
