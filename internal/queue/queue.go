// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

// Package queue implements the persistent dead-letter queue over the sqlitedb
// KV store.
//
// Each message is its own row, keyed
//
//	<queue name> || 0x00 || <8-byte big-endian time> || <8-byte big-endian seq>
//
// so enqueueing is O(1) rather than a read-modify-write of an aggregate blob,
// and fetching destroys nothing: FetchFailedMsgs only reads, and the caller
// Dequeues once the message is processed. A crash mid-cycle therefore
// re-delivers rather than loses — at-least-once.
//
// Older releases stored a whole queue as one CBOR list under the bare queue
// name; such rows are migrated on first encounter.
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

// MaxQueueAge is how long a failed message is retried before being dropped at
// fetch.
const MaxQueueAge = 30 * 24 * time.Hour

// Bounds writes detached after the caller's ctx is cancelled.
const storeTimeout = 5 * time.Second

// Separates the queue name from the sequence suffix. 0x00 cannot appear in a
// queue name, so prefix scans never cross queues.
const rowKeySep = byte(0x00)

var ErrQueueing = errors.New("problem with persistent queue")

// rowStore is a seam: splitRow's rollback runs only on a Put that fails
// partway, which a healthy store never does.
type rowStore interface {
	Put(ctx context.Context, key, value []byte) error
	Delete(ctx context.Context, key []byte) error
}

// Disambiguates rows stored in the same nanosecond; with the timestamp it makes
// keys process-unique and roughly FIFO.
var rowSeq atomic.Uint64

// rowKey builds a unique row key for the given queue.
func rowKey(queueKey []byte) []byte {
	key := make([]byte, 0, len(queueKey)+17)
	key = append(key, queueKey...)
	key = append(key, rowKeySep)
	key = binary.BigEndian.AppendUint64(key, uint64(time.Now().UnixNano()))
	key = binary.BigEndian.AppendUint64(key, rowSeq.Add(1))

	return key
}

// detachedCtx mirrors messenger.queueStoreCtx: a live ctx passes through so
// shutdown keeps propagating, a cancelled one is replaced by a detached context
// bounded by storeTimeout, so the write runs without stalling shutdown.
func detachedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}

	return context.WithTimeout(context.WithoutCancel(ctx), storeTimeout)
}

// StoreFailedMsgs appends g as its own row, so cost is independent of depth.
// QueuedAt is stamped on first failure, anchoring MaxQueueAge.
func StoreFailedMsgs(ctx context.Context, eDB *sqlitedb.Edb, key []byte, g msgtypes.Message) error {
	// Preserved, so MaxQueueAge counts from the first failure.
	if g.QueuedAt.IsZero() {
		g.QueuedAt = time.Now()
	}

	val, err := codec.EncodeMsgs([]msgtypes.Message{g})
	if err != nil {
		return err
	}

	return eDB.Put(ctx, rowKey(key), val)
}

// Dequeue removes a processed row. Call it only once the outcome is durable —
// delivered, or re-queued as a fresh row — since a crash before it re-delivers
// rather than loses. The delete survives ctx cancel, so a shutdown mid-drain
// does not duplicate the row next run.
func Dequeue(ctx context.Context, eDB rowStore, key []byte) {
	dctx, cancel := detachedCtx(ctx)
	defer cancel()

	if err := eDB.Delete(dctx, key); err != nil {
		logger.Error().Msgf("%v: %v", ErrQueueing, err)
	}
}
