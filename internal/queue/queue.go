// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

// Package queue implements the persistent failed-message (dead-letter) queue
// on top of the sqlitedb KV store.
//
// Each queued message is stored as its own row under the key
//
//	<queue name> || 0x00 || <8-byte big-endian time> || <8-byte big-endian seq>
//
// so that enqueueing is O(1) (no read-modify-write of an aggregate blob) and
// fetching never destroys data: FetchFailedMsgs only reads, and the caller
// removes each row with Dequeue after the message has been processed. A crash
// mid-cycle therefore re-delivers instead of losing messages (at-least-once).
// Older releases stored the whole queue as a single CBOR list under the bare
// queue name; FetchFailedMsgs migrates such rows to the per-message layout on
// first encounter.
package queue

import (
	"context"
	"encoding/binary"
	"errors"
	"sync/atomic"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/codec"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// MaxQueueAge caps how long a failed message is retried before being dropped at fetch.
const MaxQueueAge = 30 * 24 * time.Hour

// storeTimeout bounds detached queue writes/deletes issued after the caller's
// ctx has been cancelled (typically during shutdown).
const storeTimeout = 5 * time.Second

// rowKeySep separates the queue name from the per-message sequence suffix.
// 0x00 cannot appear in queue names, so prefix scans never cross queues.
const rowKeySep = byte(0x00)

var ErrQueueing = errors.New("problem with persistent queue")

// rowSeq disambiguates rows stored within the same nanosecond; combined with
// the timestamp it yields process-unique, roughly FIFO-ordered row keys.
var rowSeq atomic.Uint64

// rowKey builds a fresh, unique per-message row key for the given queue.
func rowKey(queueKey []byte) []byte {
	key := make([]byte, 0, len(queueKey)+17)
	key = append(key, queueKey...)
	key = append(key, rowKeySep)
	key = binary.BigEndian.AppendUint64(key, uint64(time.Now().UnixNano()))
	key = binary.BigEndian.AppendUint64(key, rowSeq.Add(1))

	return key
}

// detachedCtx mirrors messenger.queueStoreCtx: if ctx is still live, use it
// as-is so shutdown requests continue to propagate; if ctx is already
// cancelled, return a fresh context detached from cancellation but bounded by
// storeTimeout so the write still runs without stalling shutdown.
func detachedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}

	return context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
}

// StoreFailedMsgs appends g to the queue as its own row, so cost is independent
// of queue depth. QueuedAt is stamped on first failure to anchor MaxQueueAge.
func StoreFailedMsgs(ctx context.Context, eDB *sqlitedb.Edb, key []byte, g msgtypes.Message) error {
	// Preserve original QueuedAt so MaxQueueAge counts from first failure.
	if g.QueuedAt.IsZero() {
		g.QueuedAt = time.Now()
	}

	val, err := codec.EncodeMsgs([]msgtypes.Message{g})
	if err != nil {
		return err
	}

	return eDB.Put(ctx, rowKey(key), val)
}

// Dequeue removes a processed row returned by FetchFailedMsgs. Call it only
// after the outcome is durable (delivered, or re-queued as a fresh row): a
// crash before Dequeue re-delivers rather than loses. The delete survives ctx
// cancel so a shutdown mid-drain doesn't duplicate the row next run.
func Dequeue(ctx context.Context, eDB *sqlitedb.Edb, key []byte) {
	dctx, cancel := detachedCtx(ctx)
	defer cancel()

	if err := eDB.Delete(dctx, key); err != nil {
		logger.Error().Msgf("%v: %v", ErrQueueing, err)
	}
}
