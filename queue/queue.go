// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"context"
	"errors"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/encdec"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
)

// MaxQueueAge caps how long a failed message is retried before being dropped.
// Older entries are discarded at fetch time to prevent the queue from growing
// unbounded when a messenger is persistently broken.
const MaxQueueAge = 30 * 24 * time.Hour

var ErrQueueing = errors.New("problem with persistent queue")

// RequeueMsgs stores a slice of messages back into the persistent queue using a
// background context. It is intended for use when the caller's context has been
// cancelled and unprocessed messages must not be lost.
func RequeueMsgs(eDB *sqlitedb.Edb, key []byte, msgs []msgtypes.Message) {
	ctx := context.Background()

	for _, g := range msgs {
		if err := StoreFailedMsgs(ctx, eDB, key, g); err != nil {
			logger.Error().Msgf("%v: %v", ErrQueueing, err)
		}
	}
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
		msgs, err := encdec.DecodeMsgs(old)
		if err != nil {
			logger.Warn().Msgf("Failed to decode queue %q, starting fresh: %v", string(key), err)

			msgs = []msgtypes.Message{}
		}

		msgs = append(msgs, g)

		return encdec.EncodeMsgs(msgs)
	})
}
