// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/codec"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// storeLegacyAggregate writes msgs as a pre-redesign aggregate CBOR blob under
// the bare queue key, emulating a database produced by an older release.
func storeLegacyAggregate(t *testing.T, eDB *sqlitedb.Edb, queueKey []byte, msgs []msgtypes.Message) {
	t.Helper()

	encoded, err := codec.EncodeMsgs(msgs)
	if err != nil {
		t.Fatalf("EncodeMsgs failed: %v", err)
	}

	if err := eDB.FetchAndStore(context.Background(), queueKey, func(_ []byte) ([]byte, error) {
		return encoded, nil
	}); err != nil {
		t.Fatalf("FetchAndStore failed: %v", err)
	}
}

// TestFetchFailedMsgs verifies that a legacy aggregate queue row is migrated
// to per-message rows and returned on first fetch.
func TestFetchFailedMsgs(t *testing.T) {
	t.Parallel()
	// Create a temporary database for testing.
	tmpdir, err := os.MkdirTemp("", "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	eDB, err := sqlitedb.New(context.Background(), tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	queueKey := []byte("test-queue")
	storeLegacyAggregate(t, eDB, queueKey, []msgtypes.Message{
		{Username: "testuser", Subject: "Test Subject"},
	})

	// Fetch the messages
	failedMsgs := FetchFailedMsgs(context.Background(), eDB, queueKey)
	if len(failedMsgs) != 1 {
		t.Fatalf("FetchFailedMsgs() len = %d, want 1", len(failedMsgs))
	}

	if failedMsgs[0].Msg.Username != "testuser" {
		t.Errorf("failedMsgs[0].Msg.Username = %s, want testuser", failedMsgs[0].Msg.Username)
	}
}

// TestLegacyQueueMigration verifies the aggregate row is removed after
// migration so a second fetch does not duplicate messages, and that message
// order is preserved.
func TestLegacyQueueMigration(t *testing.T) {
	t.Parallel()

	tmpdir, err := os.MkdirTemp("", "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	eDB, err := sqlitedb.New(context.Background(), tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	queueKey := []byte("legacy_queue")
	storeLegacyAggregate(t, eDB, queueKey, []msgtypes.Message{
		{Subject: "Old 1", QueuedAt: time.Now()},
		{Subject: "Old 2", QueuedAt: time.Now()},
	})

	fetched := FetchFailedMsgs(context.Background(), eDB, queueKey)
	if len(fetched) != 2 {
		t.Fatalf("expected 2 migrated messages, got %d", len(fetched))
	}

	if fetched[0].Msg.Subject != "Old 1" || fetched[1].Msg.Subject != "Old 2" {
		t.Errorf("migrated messages out of order or corrupted: %v, %v",
			fetched[0].Msg.Subject, fetched[1].Msg.Subject)
	}

	// A second fetch must not duplicate: the aggregate row is gone.
	refetched := FetchFailedMsgs(context.Background(), eDB, queueKey)
	if len(refetched) != 2 {
		t.Errorf("expected 2 messages after re-fetch (no duplication), got %d", len(refetched))
	}
}

func TestFetchFailedMsgsEmptyQueue(t *testing.T) {
	t.Parallel()
	tmpdir, err := os.MkdirTemp("", "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	eDB, err := sqlitedb.New(context.Background(), tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	// Fetching from an empty (non-existent) queue key returns an empty list.
	queueKey := []byte("test-empty-queue")
	failedMsgs := FetchFailedMsgs(context.Background(), eDB, queueKey)

	if len(failedMsgs) != 0 {
		t.Errorf("FetchFailedMsgs() on empty queue should return empty list, got %d msgs", len(failedMsgs))
	}
}

func TestFetchFailedMsgsCorruptedData(t *testing.T) {
	t.Parallel()
	tmpdir, err := os.MkdirTemp("", "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	eDB, err := sqlitedb.New(context.Background(), tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	queueKey := []byte("test-corrupted-queue")

	// Inject corrupted data directly into the legacy aggregate row.
	if err := eDB.FetchAndStore(context.Background(), queueKey, func(_ []byte) ([]byte, error) {
		return []byte("this is not valid cbor data"), nil
	}); err != nil {
		t.Fatalf("FetchAndStore failed: %v", err)
	}

	// Should return empty list on corrupted data without panicking.
	failedMsgs := FetchFailedMsgs(context.Background(), eDB, queueKey)
	if len(failedMsgs) != 0 {
		t.Errorf("FetchFailedMsgs() on corrupted queue should return empty list, got %d msgs", len(failedMsgs))
	}
}
