// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"context"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/codec"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// Queued couples a fetched message with the row key it was read from so the
// caller can Dequeue exactly that row once the message has been processed.
type Queued struct {
	Key []byte
	Msg msgtypes.Message
}

// FetchFailedMsgs returns the failed messages queued under queueKey, oldest
// first, without removing them: rows stay in the database until the caller
// confirms processing via Dequeue, so a crash mid-resend re-delivers instead
// of losing messages.
//
// Messages older than MaxQueueAge are dropped (their rows deleted) here.
// Legacy aggregate-blob queues written by older releases are transparently
// split into per-message rows on first encounter.
//
// If any of the operations fail, the function logs an error and returns
// whatever could be read.
func FetchFailedMsgs(ctx context.Context, eDB *sqlitedb.Edb, queueKey []byte) []Queued {
	queueKeyStr := string(queueKey)

	migrateLegacyQueue(ctx, eDB, queueKey)

	prefix := make([]byte, 0, len(queueKey)+1)
	prefix = append(prefix, queueKey...)
	prefix = append(prefix, rowKeySep)

	rows, err := eDB.ScanPrefix(ctx, prefix)
	if err != nil {
		logger.Error().Msgf("Error reading failed messages for queue %v: %v", queueKeyStr, err)

		return nil
	}

	now := time.Now()
	kept := make([]Queued, 0, len(rows))
	dropped := 0

	for _, row := range rows {
		msgs, decErr := codec.DecodeMsgs(row.Value)
		if decErr != nil {
			logger.Warn().Msgf("Failed to decode queue %q row, dropping it: %v", queueKeyStr, decErr)

			Dequeue(ctx, eDB, row.Key)

			continue
		}

		// Rows normally hold exactly one message; tolerate more defensively.
		for _, m := range msgs {
			if !m.QueuedAt.IsZero() && now.Sub(m.QueuedAt) > MaxQueueAge {
				dropped++

				Dequeue(ctx, eDB, row.Key)

				continue
			}

			// Stamp legacy zero QueuedAt so MaxQueueAge applies from now on.
			if m.QueuedAt.IsZero() {
				m.QueuedAt = now
			}

			kept = append(kept, Queued{Msg: m, Key: row.Key})
		}
	}

	if dropped > 0 {
		logger.Warn().Msgf("Dropped %v messages older than %v from queue %v", dropped, MaxQueueAge, queueKeyStr)
	}

	if len(kept) > 0 {
		logger.Info().Msgf("Found %v failed messages in queue %v, trying to resend", len(kept), queueKeyStr)
	}

	return kept
}

// migrateLegacyQueue splits a pre-redesign aggregate queue row (whole queue
// as one CBOR list stored under the bare queue name) into per-message rows.
// The aggregate row is deleted only after every message has been re-stored,
// so a crash mid-migration duplicates rather than loses; the migration is
// idempotent apart from those duplicates.
func migrateLegacyQueue(ctx context.Context, eDB *sqlitedb.Edb, queueKey []byte) {
	var legacy []msgtypes.Message

	// Read-only peek: returning old unchanged skips the write. A row that no
	// longer decodes is unrecoverable — delete it (return empty) so it does
	// not linger and re-warn on every subsequent fetch.
	err := eDB.FetchAndStore(ctx, queueKey, func(old []byte) ([]byte, error) {
		var decErr error

		legacy, decErr = codec.DecodeMsgs(old)
		if decErr != nil {
			logger.Warn().Msgf("Failed to decode legacy queue %q, discarding it: %v", string(queueKey), decErr)

			legacy = nil

			return []byte{}, nil
		}

		return old, nil
	})
	if err != nil {
		logger.Error().Msgf("Error checking legacy queue %v: %v", string(queueKey), err)

		return
	}

	if len(legacy) == 0 {
		return
	}

	logger.Info().Msgf("Migrating %v messages from legacy queue %v to per-message rows", len(legacy), string(queueKey))

	for _, m := range legacy {
		if err := StoreFailedMsgs(ctx, eDB, queueKey, m); err != nil {
			logger.Error().Msgf("%v: %v", ErrQueueing, err)

			// Keep the aggregate row so nothing is lost; retry next fetch.
			return
		}
	}

	if err := eDB.Delete(ctx, queueKey); err != nil {
		logger.Error().Msgf("%v: %v", ErrQueueing, err)
	}
}
