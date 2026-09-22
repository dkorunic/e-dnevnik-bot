// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"context"
	"sync"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/codec"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// Queues whose pre-redesign aggregate row is migrated or proven absent, so the
// probe transaction is paid once per process rather than every cycle.
var legacyChecked sync.Map // map[string]struct{}, keyed by queue name

// Queued pairs a message with its row key, so the caller can Dequeue exactly
// that row once it is processed.
type Queued struct {
	Key []byte
	Msg msgtypes.Message
}

// FetchFailedMsgs returns queueKey's messages oldest first without removing
// them: rows survive until the caller Dequeues, so a crash mid-resend
// re-delivers rather than loses.
//
// Messages past MaxQueueAge are dropped here. Legacy aggregate rows are split
// into per-message rows on first encounter, as is any multi-message row, so
// every returned Queued has a unique key.
//
// A failure logs and returns whatever could be read.
func FetchFailedMsgs(ctx context.Context, eDB *sqlitedb.Edb, queueKey []byte) []Queued {
	queueKeyStr := string(queueKey)

	// Once per process per queue; a failed migration reports not-done and
	// retries on the next fetch.
	if _, done := legacyChecked.Load(queueKeyStr); !done {
		if migrateLegacyQueue(ctx, eDB, queueKey) {
			legacyChecked.Store(queueKeyStr, struct{}{})
		}
	}

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

		// Classify the whole row first: a legacy row may hold several messages,
		// and deleting on the first expiry would orphan live siblings.
		survivors := make([]msgtypes.Message, 0, len(msgs))

		rowDropped := 0
		rowStamped := false

		for _, m := range msgs {
			if !m.QueuedAt.IsZero() && now.Sub(m.QueuedAt) > MaxQueueAge {
				rowDropped++

				continue
			}

			// Stamp a legacy zero QueuedAt so MaxQueueAge applies. It must be
			// persisted below, or every fetch re-bases the age and the row
			// never expires.
			if m.QueuedAt.IsZero() {
				m.QueuedAt = now
				rowStamped = true
			}

			survivors = append(survivors, m)
		}

		dropped += rowDropped

		// Whole row expired.
		if len(survivors) == 0 {
			Dequeue(ctx, eDB, row.Key)

			continue
		}

		// Split so the caller's per-message Dequeue stays crash-safe: under a
		// shared key the first Dequeue would orphan live siblings. A failed
		// split leaves the original intact and skips the row this cycle.
		if len(survivors) > 1 {
			newKeys := splitRow(ctx, eDB, queueKey, row.Key, survivors)
			if newKeys == nil {
				continue
			}

			for i, m := range survivors {
				kept = append(kept, Queued{Msg: m, Key: newKeys[i]})
			}

			continue
		}

		// Rewrite in place only if expiry or a stamp changed the contents.
		if rowDropped > 0 || rowStamped {
			if val, encErr := codec.EncodeMsgs(survivors); encErr != nil {
				logger.Error().Msgf("%v: %v", ErrQueueing, encErr)
			} else if putErr := eDB.Put(ctx, row.Key, val); putErr != nil {
				logger.Error().Msgf("%v: %v", ErrQueueing, putErr)
			}
		}

		kept = append(kept, Queued{Msg: survivors[0], Key: row.Key})
	}

	if dropped > 0 {
		logger.Warn().Msgf("Dropped %v messages older than %v from queue %v", dropped, MaxQueueAge, queueKeyStr)
	}

	if len(kept) > 0 {
		logger.Info().Msgf("Found %v failed messages in queue %v, trying to resend", len(kept), queueKeyStr)
	}

	return kept
}

// splitRow rewrites a multi-message row as one row per message and removes the
// original, returning the new keys in survivor order. Any failure rolls the
// partial split back and returns nil, so the caller skips the row — no loss, no
// duplication.
func splitRow(ctx context.Context, eDB rowStore, queueKey, origKey []byte, survivors []msgtypes.Message) [][]byte {
	newKeys := make([][]byte, 0, len(survivors))

	for _, m := range survivors {
		val, err := codec.EncodeMsgs([]msgtypes.Message{m})
		if err == nil {
			key := rowKey(queueKey)
			if err = eDB.Put(ctx, key, val); err == nil {
				newKeys = append(newKeys, key)

				continue
			}
		}

		logger.Error().Msgf("%v: %v", ErrQueueing, err)

		for _, k := range newKeys {
			Dequeue(ctx, eDB, k)
		}

		return nil
	}

	Dequeue(ctx, eDB, origKey)

	return newKeys
}

// migrateLegacyQueue splits a pre-redesign aggregate row into per-message rows,
// deleting the aggregate only once every message is re-stored — a crash
// mid-migration duplicates rather than loses.
//
// Reports whether the legacy row is gone, absent, undecodable or migrated alike,
// so the caller can latch the probe.
func migrateLegacyQueue(ctx context.Context, eDB *sqlitedb.Edb, queueKey []byte) bool {
	var legacy []msgtypes.Message

	// Returning old unchanged skips the write. A row that no longer decodes
	// is unrecoverable, so returning empty deletes it rather than let it
	// re-warn on every fetch.
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

		return false
	}

	if len(legacy) == 0 {
		return true
	}

	logger.Info().Msgf("Migrating %v messages from legacy queue %v to per-message rows", len(legacy), string(queueKey))

	for _, m := range legacy {
		if err := StoreFailedMsgs(ctx, eDB, queueKey, m); err != nil {
			logger.Error().Msgf("%v: %v", ErrQueueing, err)

			// Keep the aggregate so nothing is lost; retry next fetch.
			return false
		}
	}

	if err := eDB.Delete(ctx, queueKey); err != nil {
		// Already migrated, and the leftover aggregate is invisible to the
		// prefix scan. Re-migrating would duplicate, so this still counts
		// as done.
		logger.Error().Msgf("%v: %v", ErrQueueing, err)
	}

	return true
}
