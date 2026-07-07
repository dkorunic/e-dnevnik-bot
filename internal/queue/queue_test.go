// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

func TestStoreAndFetchFailedMsgs(t *testing.T) {
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

	key := []byte("test_queue")
	msg1 := msgtypes.Message{Subject: "Test Subject 1", Descriptions: []string{"Test Body 1"}}
	msg2 := msgtypes.Message{Subject: "Test Subject 2", Descriptions: []string{"Test Body 2"}}

	if err := StoreFailedMsgs(context.Background(), eDB, key, msg1); err != nil {
		t.Fatalf("StoreFailedMsgs failed: %v", err)
	}

	if err := StoreFailedMsgs(context.Background(), eDB, key, msg2); err != nil {
		t.Fatalf("StoreFailedMsgs failed: %v", err)
	}

	fetchedMsgs := FetchFailedMsgs(context.Background(), eDB, key)

	// QueuedAt is stamped on enqueue; verify and clear before payload compare.
	now := time.Now()

	msgs := make([]msgtypes.Message, 0, len(fetchedMsgs))

	for i := range fetchedMsgs {
		if fetchedMsgs[i].Msg.QueuedAt.IsZero() {
			t.Errorf("fetched message %d has zero QueuedAt; expected it to be stamped on enqueue", i)
		}

		if d := now.Sub(fetchedMsgs[i].Msg.QueuedAt); d < 0 || d > time.Minute {
			t.Errorf("fetched message %d has QueuedAt outside expected window: %v", i, fetchedMsgs[i].Msg.QueuedAt)
		}

		m := fetchedMsgs[i].Msg
		m.QueuedAt = time.Time{}
		msgs = append(msgs, m)
	}

	// Row keys are time-ordered, so FIFO order is preserved.
	expectedMsgs := []msgtypes.Message{msg1, msg2}

	if !reflect.DeepEqual(msgs, expectedMsgs) {
		t.Errorf("fetched messages do not match expected messages.\nGot: %v\nWant: %v", msgs, expectedMsgs)
	}

	// Fetch must NOT drain: rows stay until the caller confirms processing.
	refetched := FetchFailedMsgs(context.Background(), eDB, key)
	if len(refetched) != len(fetchedMsgs) {
		t.Errorf("fetch should not drain the queue; want %d msgs, got %d", len(fetchedMsgs), len(refetched))
	}

	// Dequeue removes exactly the processed rows.
	for _, q := range fetchedMsgs {
		Dequeue(context.Background(), eDB, q.Key)
	}

	if remaining := FetchFailedMsgs(context.Background(), eDB, key); len(remaining) != 0 {
		t.Errorf("queue should be empty after dequeueing all rows, got: %v", remaining)
	}
}

func TestStoreFailedMsgsCorruptedQueue(t *testing.T) {
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

	key := []byte("test_corrupted_queue")

	// Inject corrupted bytes into the legacy aggregate row to simulate a broken queue.
	if err := eDB.FetchAndStore(context.Background(), key, func(_ []byte) ([]byte, error) {
		return []byte("corrupted cbor data"), nil
	}); err != nil {
		t.Fatalf("FetchAndStore failed: %v", err)
	}

	// StoreFailedMsgs writes fresh per-message rows regardless of the corrupted legacy row.
	newMsg := msgtypes.Message{Subject: "Recovery Test"}
	if err := StoreFailedMsgs(context.Background(), eDB, key, newMsg); err != nil {
		t.Fatalf("StoreFailedMsgs() on corrupted queue should not fail: %v", err)
	}

	fetchedMsgs := FetchFailedMsgs(context.Background(), eDB, key)
	if len(fetchedMsgs) != 1 {
		t.Errorf("expected 1 message after recovery, got %d", len(fetchedMsgs))
	}

	if len(fetchedMsgs) > 0 && fetchedMsgs[0].Msg.Subject != "Recovery Test" {
		t.Errorf("unexpected message subject: %s", fetchedMsgs[0].Msg.Subject)
	}
}

// TestQueueAging verifies messages older than MaxQueueAge are dropped (and
// their rows deleted) at fetch.
func TestQueueAging(t *testing.T) {
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

	key := []byte("aging_queue")

	stale := msgtypes.Message{Subject: "Stale", QueuedAt: time.Now().Add(-MaxQueueAge - time.Hour)}
	fresh := msgtypes.Message{Subject: "Fresh"}

	if err := StoreFailedMsgs(context.Background(), eDB, key, stale); err != nil {
		t.Fatalf("StoreFailedMsgs failed: %v", err)
	}

	if err := StoreFailedMsgs(context.Background(), eDB, key, fresh); err != nil {
		t.Fatalf("StoreFailedMsgs failed: %v", err)
	}

	fetched := FetchFailedMsgs(context.Background(), eDB, key)
	if len(fetched) != 1 {
		t.Fatalf("expected only the fresh message, got %d", len(fetched))
	}

	if fetched[0].Msg.Subject != "Fresh" {
		t.Errorf("expected Fresh, got %v", fetched[0].Msg.Subject)
	}

	// The stale row must be gone for good, not just filtered.
	refetched := FetchFailedMsgs(context.Background(), eDB, key)
	if len(refetched) != 1 {
		t.Errorf("expected 1 message after re-fetch, got %d", len(refetched))
	}
}

// TestQueueIsolation verifies that per-message rows of one queue are invisible
// to another queue whose name shares a prefix.
func TestQueueIsolation(t *testing.T) {
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

	if err := StoreFailedMsgs(context.Background(), eDB, []byte("queue"), msgtypes.Message{Subject: "A"}); err != nil {
		t.Fatal(err)
	}

	if err := StoreFailedMsgs(context.Background(), eDB, []byte("queue-two"), msgtypes.Message{Subject: "B"}); err != nil {
		t.Fatal(err)
	}

	if got := FetchFailedMsgs(context.Background(), eDB, []byte("queue")); len(got) != 1 || got[0].Msg.Subject != "A" {
		t.Errorf("queue %q returned wrong rows: %v", "queue", got)
	}

	if got := FetchFailedMsgs(context.Background(), eDB, []byte("queue-two")); len(got) != 1 || got[0].Msg.Subject != "B" {
		t.Errorf("queue %q returned wrong rows: %v", "queue-two", got)
	}
}
