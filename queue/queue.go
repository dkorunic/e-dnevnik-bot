// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"context"
	"errors"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/codec"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
)

// MaxQueueAge caps how long a failed message is retried before being dropped.
// Older entries are discarded at fetch time to prevent the queue from growing
// unbounded when a messenger is persistently broken.
const MaxQueueAge = 30 * 24 * time.Hour

// requeueTimeout bounds the total time RequeueMsgs spends persisting the
// tail-slice of un-delivered messages on shutdown. The whole batch shares one
// budget so a slow sqlite write cannot stall shutdown proportionally to the
// number of messages still in flight. Matches messenger.storeTimeout in spirit.
const requeueTimeout = 5 * time.Second

var ErrQueueing = errors.New("problem with persistent queue")

// RequeueMsgs stores msgs back into the persistent queue when the caller's
// drain loop was interrupted (typically by shutdown). The caller's ctx is
// usually already cancelled at this point; requeueCtx returns a detached,
// time-bounded context so every write still runs while the whole batch is
// capped at requeueTimeout regardless of queue depth or sqlite latency.
func RequeueMsgs(ctx context.Context, eDB *sqlitedb.Edb, key []byte, msgs []msgtypes.Message) {
	storeCtx, cancel := requeueCtx(ctx)
	defer cancel()

	for _, g := range msgs {
		if err := StoreFailedMsgs(storeCtx, eDB, key, g); err != nil {
			logger.Error().Msgf("%v: %v", ErrQueueing, err)
		}
	}
}

// requeueCtx mirrors messenger.queueStoreCtx: if ctx is still live, use it
// as-is so shutdown requests continue to propagate; if ctx is already
// cancelled, return a fresh context detached from cancellation but bounded by
// requeueTimeout so the write still runs without stalling shutdown.
func requeueCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}

	return context.WithTimeout(context.WithoutCancel(ctx), requeueTimeout)
}

// StoreFailedMsgs stores a message in a persistent queue identified by key.
// The message is appended to any existing messages in the queue, and the queue
// is stored in the database using the given key.
//
// The function assumes the database and the key are valid. If the key doesn't
// exist, it will be created.
//
// If any of the operations fail, the function returns an error.
func StoreFailedMsgs(ctx context.Context, eDB *sqlitedb.Edb, key []byte, g msgtypes.Message) error {
	// Preserve original QueuedAt so MaxQueueAge counts from first failure.
	if g.QueuedAt.IsZero() {
		g.QueuedAt = time.Now()
	}

	return eDB.FetchAndStore(ctx, key, func(old []byte) ([]byte, error) {
		msgs, err := codec.DecodeMsgs(old)
		if err != nil {
			logger.Warn().Msgf("Failed to decode queue %q, starting fresh: %v", string(key), err)

			msgs = []msgtypes.Message{}
		}

		msgs = append(msgs, g)

		return codec.EncodeMsgs(msgs)
	})
}
