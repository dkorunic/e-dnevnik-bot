// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package sqlitedb

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func assertPrivate(t *testing.T, path string) {
	t.Helper()

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		fi, err := os.Stat(p)
		if errors.Is(err, fs.ErrNotExist) && p != path {
			continue
		}

		if err != nil {
			t.Fatalf("Stat(%q) = %v", p, err)
		}

		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%v mode = %o, want 600", filepath.Base(p), got)
		}
	}
}

// The store holds queued messages with children's grades, and the default
// umask would leave them readable by every local user.
func TestNewCreatesPrivateFiles(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "new.db.sqlite")

	eDB, err := New(t.Context(), path)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	if err := eDB.Put(t.Context(), []byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	assertPrivate(t, path)
}

// Installs from before this existed have a 0644 store already.
func TestNewTightensExistingFiles(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "old.db.sqlite")

	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.WriteFile(p, nil, 0o644); err != nil { //nolint:gosec // the mode under test
			t.Fatalf("WriteFile() = %v", err)
		}
	}

	eDB, err := New(t.Context(), path)
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	assertPrivate(t, path)
}

func TestCheckAndFlagOutbox(t *testing.T) {
	t.Parallel()

	eDB, err := New(t.Context(), filepath.Join(t.TempDir(), "outbox.db.sqlite"))
	if err != nil {
		t.Fatalf("New() = %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	ctx := context.Background()
	key, val := []byte("outbox\x00a"), []byte("payload")

	rowStored := func() bool {
		rows, err := eDB.ScanPrefix(ctx, key)
		if err != nil {
			t.Fatalf("ScanPrefix() = %v", err)
		}

		return len(rows) == 1
	}

	// A failing row must leave nothing flagged, or the event is seen and never
	// forwarded.
	boom := errors.New("boom")
	if _, err := eDB.CheckAndFlagOutbox(ctx, "u", "s", []string{"1"}, func() ([]byte, []byte, error) { return nil, nil, boom }); !errors.Is(err, boom) {
		t.Fatalf("failing row: err = %v, want boom", err)
	}

	found, err := eDB.CheckAndFlagOutbox(ctx, "u", "s", []string{"1"}, func() ([]byte, []byte, error) { return key, val, nil })
	if err != nil || found {
		t.Fatalf("new event: found = %v, err = %v, want false after the failed attempt rolled back", found, err)
	}

	if !rowStored() {
		t.Error("new event: the outbox row was not stored with the flag")
	}

	found, err = eDB.CheckAndFlagOutbox(ctx, "u", "s", []string{"1"}, func() ([]byte, []byte, error) {
		t.Error("row asked for an event already seen")

		return nil, nil, nil
	})
	if err != nil || !found {
		t.Errorf("seen event: found = %v, err = %v, want true", found, err)
	}

	// Flagged but deliberately not forwarded: suppressed, stale, first run.
	found, err = eDB.CheckAndFlagOutbox(ctx, "u", "s", []string{"2"}, func() ([]byte, []byte, error) { return nil, nil, nil })
	if err != nil || found {
		t.Errorf("unforwarded event: found = %v, err = %v, want false", found, err)
	}

	if found, _ = eDB.CheckAndFlagTTL(ctx, "u", "s", []string{"2"}); !found {
		t.Error("unforwarded event was not flagged")
	}
}
