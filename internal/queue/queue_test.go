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
	for i := range fetchedMsgs {
		if fetchedMsgs[i].QueuedAt.IsZero() {
			t.Errorf("fetched message %d has zero QueuedAt; expected it to be stamped on enqueue", i)
		}

		if d := now.Sub(fetchedMsgs[i].QueuedAt); d < 0 || d > time.Minute {
			t.Errorf("fetched message %d has QueuedAt outside expected window: %v", i, fetchedMsgs[i].QueuedAt)
		}

		fetchedMsgs[i].QueuedAt = time.Time{}
	}

	expectedMsgs := []msgtypes.Message{msg1, msg2}

	if !reflect.DeepEqual(fetchedMsgs, expectedMsgs) {
		t.Errorf("fetched messages do not match expected messages.\nGot: %v\nWant: %v", fetchedMsgs, expectedMsgs)
	}

	// Fetch drains the queue.
	fetchedMsgs = FetchFailedMsgs(context.Background(), eDB, key)
	if len(fetchedMsgs) != 0 {
		t.Errorf("queue should be empty after fetching, but it's not. Got: %v", fetchedMsgs)
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

	// Inject corrupted bytes to simulate a broken queue.
	if err := eDB.FetchAndStore(context.Background(), key, func(_ []byte) ([]byte, error) {
		return []byte("corrupted gob data"), nil
	}); err != nil {
		t.Fatalf("FetchAndStore failed: %v", err)
	}

	// StoreFailedMsgs must start fresh on corrupted data.
	newMsg := msgtypes.Message{Subject: "Recovery Test"}
	if err := StoreFailedMsgs(context.Background(), eDB, key, newMsg); err != nil {
		t.Fatalf("StoreFailedMsgs() on corrupted queue should not fail: %v", err)
	}

	fetchedMsgs := FetchFailedMsgs(context.Background(), eDB, key)
	if len(fetchedMsgs) != 1 {
		t.Errorf("expected 1 message after recovery, got %d", len(fetchedMsgs))
	}

	if len(fetchedMsgs) > 0 && fetchedMsgs[0].Subject != "Recovery Test" {
		t.Errorf("unexpected message subject: %s", fetchedMsgs[0].Subject)
	}
}
