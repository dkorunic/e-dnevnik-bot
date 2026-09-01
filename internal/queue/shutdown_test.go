// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// newShutdownTestDB opens a throwaway database.
func newShutdownTestDB(t *testing.T) *sqlitedb.Edb {
	t.Helper()

	eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), "queue.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	return eDB
}

// TestDetachedCtxPassesLiveContextThrough: while the process is running
// normally, queue writes must stay cancellable so a shutdown request still
// propagates. Detaching unconditionally would make every write ignore SIGTERM.
func TestDetachedCtxPassesLiveContextThrough(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	got, gotCancel := detachedCtx(ctx)
	defer gotCancel()

	if got != ctx {
		t.Fatal("detachedCtx replaced a live context; cancellation would stop propagating")
	}

	cancel()

	if got.Err() == nil {
		t.Error("cancelling the parent did not cancel the returned context")
	}
}

// TestDetachedCtxRevivesCancelledContext is the shutdown-tolerance contract:
// once the parent is cancelled the returned context must be live again, so the
// final write of an already-dedup-flagged message still lands. It must also
// carry a deadline, or a hung write would stall shutdown indefinitely.
func TestDetachedCtxRevivesCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	got, gotCancel := detachedCtx(ctx)
	defer gotCancel()

	if got.Err() != nil {
		t.Fatalf("returned context is already done (%v); the final queue write would be skipped", got.Err())
	}

	deadline, ok := got.Deadline()
	if !ok {
		t.Fatal("returned context has no deadline; a stalled write would hold shutdown open forever")
	}

	if d := time.Until(deadline); d <= 0 || d > storeTimeout+time.Second {
		t.Errorf("deadline is %v away, want roughly storeTimeout (%v)", d, storeTimeout)
	}
}

// TestStoreFailedMsgsHonoursCancellation pins the layering. StoreFailedMsgs
// takes the caller's context verbatim and lets cancellation through — it does
// *not* detach internally. Shutdown-tolerance is the caller's decision, applied
// by wrapping with detachedCtx (or messenger.queueStoreCtx). Making the write
// detach unconditionally here would take that choice away and leave callers
// unable to abort a queue write at all.
func TestStoreFailedMsgsHonoursCancellation(t *testing.T) {
	t.Parallel()

	eDB := newShutdownTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	queueKey := []byte("shutdown-store-raw")

	if err := StoreFailedMsgs(ctx, eDB, queueKey, msgtypes.Message{Subject: "Kemija"}); err == nil {
		t.Error("StoreFailedMsgs() with a cancelled context = nil; cancellation must propagate so callers stay in control")
	}

	if got := FetchFailedMsgs(context.Background(), eDB, queueKey); len(got) != 0 {
		t.Errorf("FetchFailedMsgs = %+v, want nothing written on a cancelled context", got)
	}
}

// TestStoreFailedMsgsSurvivesShutdownWhenWrapped is the other half of that
// contract, and the one that matters in production: a SIGTERM arriving mid-send
// must not cost the message. It is already flagged in the dedup store and will
// never be re-scraped, so this write is the only thing between it and permanent
// loss — which is why every messenger wraps the call before making it.
func TestStoreFailedMsgsSurvivesShutdownWhenWrapped(t *testing.T) {
	t.Parallel()

	eDB := newShutdownTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	queueKey := []byte("shutdown-store-wrapped")

	sctx, scancel := detachedCtx(ctx)
	defer scancel()

	if err := StoreFailedMsgs(sctx, eDB, queueKey, msgtypes.Message{Subject: "Kemija"}); err != nil {
		t.Fatalf("StoreFailedMsgs() through detachedCtx = %v, want nil", err)
	}

	got := FetchFailedMsgs(context.Background(), eDB, queueKey)
	if len(got) != 1 || got[0].Msg.Subject != "Kemija" {
		t.Fatalf("FetchFailedMsgs = %+v, want the message persisted despite a cancelled parent", got)
	}
}

// TestDequeueDuringShutdown: the delete must also survive cancellation. If a
// processed row outlives the shutdown that removed it, the next run re-delivers
// it and the user gets a duplicate alert.
func TestDequeueDuringShutdown(t *testing.T) {
	t.Parallel()

	eDB := newShutdownTestDB(t)
	queueKey := []byte("shutdown-dequeue")

	if err := StoreFailedMsgs(context.Background(), eDB, queueKey, msgtypes.Message{Subject: "Fizika"}); err != nil {
		t.Fatalf("StoreFailedMsgs() failed: %v", err)
	}

	rows := FetchFailedMsgs(context.Background(), eDB, queueKey)
	if len(rows) != 1 {
		t.Fatalf("FetchFailedMsgs returned %d rows, want 1", len(rows))
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	Dequeue(ctx, eDB, rows[0].Key)

	if got := FetchFailedMsgs(context.Background(), eDB, queueKey); len(got) != 0 {
		t.Errorf("row survived a Dequeue issued during shutdown (%+v); it would be re-delivered as a duplicate", got)
	}
}

// TestStoreFailedMsgsPreservesQueuedAt: MaxQueueAge must count from the *first*
// failure. Re-stamping QueuedAt on every retry would make a permanently
// undeliverable message immortal, retried every cycle forever.
func TestStoreFailedMsgsPreservesQueuedAt(t *testing.T) {
	t.Parallel()

	eDB := newShutdownTestDB(t)
	queueKey := []byte("queued-at-preserved")

	firstFailure := time.Now().Add(-MaxQueueAge / 2).Round(0)

	if err := StoreFailedMsgs(context.Background(), eDB, queueKey,
		msgtypes.Message{Subject: "Povijest", QueuedAt: firstFailure}); err != nil {
		t.Fatalf("StoreFailedMsgs() failed: %v", err)
	}

	got := FetchFailedMsgs(context.Background(), eDB, queueKey)
	if len(got) != 1 {
		t.Fatalf("FetchFailedMsgs returned %d rows, want 1", len(got))
	}

	if !got[0].Msg.QueuedAt.Equal(firstFailure) {
		t.Errorf("QueuedAt = %v, want the original %v — re-stamping on requeue would stop MaxQueueAge from ever expiring the row",
			got[0].Msg.QueuedAt, firstFailure)
	}
}

// TestStoreFailedMsgsStampsZeroQueuedAt is the complement: a message entering
// the queue for the first time carries no QueuedAt, and must be stamped so
// MaxQueueAge has a clock to count from.
func TestStoreFailedMsgsStampsZeroQueuedAt(t *testing.T) {
	t.Parallel()

	eDB := newShutdownTestDB(t)
	queueKey := []byte("queued-at-stamped")

	before := time.Now()

	if err := StoreFailedMsgs(context.Background(), eDB, queueKey,
		msgtypes.Message{Subject: "Biologija"}); err != nil {
		t.Fatalf("StoreFailedMsgs() failed: %v", err)
	}

	got := FetchFailedMsgs(context.Background(), eDB, queueKey)
	if len(got) != 1 {
		t.Fatalf("FetchFailedMsgs returned %d rows, want 1", len(got))
	}

	stamped := got[0].Msg.QueuedAt
	if stamped.IsZero() {
		t.Fatal("QueuedAt is zero; MaxQueueAge would never expire this row")
	}

	if stamped.Before(before.Add(-time.Second)) || stamped.After(time.Now().Add(time.Second)) {
		t.Errorf("QueuedAt = %v, want a stamp from around now (%v)", stamped, before)
	}
}

// TestRowKeysAreUniqueUnderConcurrency: row keys combine a nanosecond timestamp
// with an atomic sequence precisely because several messengers queue at once. A
// collision would silently overwrite one messenger's undelivered message with
// another's.
func TestRowKeysAreUniqueUnderConcurrency(t *testing.T) {
	t.Parallel()

	eDB := newShutdownTestDB(t)
	queueKey := []byte("concurrent-rows")

	const n = 64

	errs := make(chan error, n)

	for i := range n {
		go func() {
			errs <- StoreFailedMsgs(context.Background(), eDB, queueKey,
				msgtypes.Message{Subject: "Subject", Fields: []string{string(rune('a' + i%26))}})
		}()
	}

	for range n {
		if err := <-errs; err != nil {
			t.Fatalf("StoreFailedMsgs() failed: %v", err)
		}
	}

	if got := FetchFailedMsgs(context.Background(), eDB, queueKey); len(got) != n {
		t.Errorf("queue holds %d rows after %d concurrent stores, want %d — row keys collided and overwrote messages",
			len(got), n, n)
	}
}
