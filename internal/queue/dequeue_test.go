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

// TestFetchFailedMsgsPartialExpiryKeepsSurvivor: an expired message in a
// multi-message row must not delete the whole row and lose its live siblings;
// the rewritten row still returns the survivor on a second fetch.
func TestFetchFailedMsgsPartialExpiryKeepsSurvivor(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	eDB, err := sqlitedb.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close() //nolint:errcheck

	queueKey := []byte("test-queue")

	// One row holding an expired message followed by a live one.
	encoded, err := codec.EncodeMsgs([]msgtypes.Message{
		{Subject: "expired", QueuedAt: time.Now().Add(-2 * MaxQueueAge)},
		{Subject: "live", QueuedAt: time.Now()},
	})
	if err != nil {
		t.Fatalf("EncodeMsgs failed: %v", err)
	}

	if err := eDB.Put(ctx, rowKey(queueKey), encoded); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	got := FetchFailedMsgs(ctx, eDB, queueKey)
	if len(got) != 1 || got[0].Msg.Subject != "live" {
		t.Fatalf("first fetch = %+v, want single 'live' survivor", got)
	}

	// Survivor must persist (row rewritten, not deleted): second fetch returns it.
	got2 := FetchFailedMsgs(ctx, eDB, queueKey)
	if len(got2) != 1 || got2[0].Msg.Subject != "live" {
		t.Fatalf("second fetch = %+v, want survivor still present (not lost)", got2)
	}
}

// TestFetchFailedMsgsMultiSurvivorSplit: a multi-message row must be split
// into per-message rows at fetch, so a crash after dequeuing one survivor
// cannot orphan its siblings (shared key would delete the row for all).
func TestFetchFailedMsgsMultiSurvivorSplit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	eDB, err := sqlitedb.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close() //nolint:errcheck

	queueKey := []byte("test-queue")

	encoded, err := codec.EncodeMsgs([]msgtypes.Message{
		{Subject: "one", QueuedAt: time.Now()},
		{Subject: "two", QueuedAt: time.Now()},
	})
	if err != nil {
		t.Fatalf("EncodeMsgs failed: %v", err)
	}

	if err := eDB.Put(ctx, rowKey(queueKey), encoded); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	got := FetchFailedMsgs(ctx, eDB, queueKey)
	if len(got) != 2 {
		t.Fatalf("fetch = %d messages, want 2", len(got))
	}

	if string(got[0].Key) == string(got[1].Key) {
		t.Fatal("survivors share a row key; multi-message row was not split")
	}

	if got[0].Msg.Subject != "one" || got[1].Msg.Subject != "two" {
		t.Fatalf("fetch order = %q, %q, want one, two", got[0].Msg.Subject, got[1].Msg.Subject)
	}

	// Simulate a crash after processing the first survivor: dequeue it, then
	// re-fetch — the sibling must still be there.
	Dequeue(ctx, eDB, got[0].Key)

	refetched := FetchFailedMsgs(ctx, eDB, queueKey)
	if len(refetched) != 1 || refetched[0].Msg.Subject != "two" {
		t.Fatalf("re-fetch = %+v, want lone sibling 'two' (orphaned by shared key?)", refetched)
	}
}

// TestFetchFailedMsgsAllExpiredDropsRow verifies a row whose every message is
// past MaxQueueAge is removed and returns nothing.
func TestFetchFailedMsgsAllExpiredDropsRow(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	eDB, err := sqlitedb.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close() //nolint:errcheck

	queueKey := []byte("test-queue")

	encoded, err := codec.EncodeMsgs([]msgtypes.Message{
		{Subject: "expired", QueuedAt: time.Now().Add(-2 * MaxQueueAge)},
	})
	if err != nil {
		t.Fatalf("EncodeMsgs failed: %v", err)
	}

	if err := eDB.Put(ctx, rowKey(queueKey), encoded); err != nil {
		t.Fatalf("Put failed: %v", err)
	}

	if got := FetchFailedMsgs(ctx, eDB, queueKey); len(got) != 0 {
		t.Fatalf("fetch = %+v, want empty (all expired)", got)
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
