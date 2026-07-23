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

// legacyChecked tracks queues whose pre-redesign aggregate row has been
// migrated or proven absent this process, so FetchFailedMsgs skips the probe
// transaction after the first pass instead of paying it per messenger per
// cycle forever.
var legacyChecked sync.Map // map[string]struct{}, keyed by queue name

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
// split into per-message rows on first encounter; any multi-message row is
// also split so every returned Queued has a unique key.
//
// If any of the operations fail, the function logs an error and returns
// whatever could be read.
func FetchFailedMsgs(ctx context.Context, eDB *sqlitedb.Edb, queueKey []byte) []Queued {
	queueKeyStr := string(queueKey)

	// Probe for a legacy aggregate row once per process per queue; a failed
	// migration reports not-done and is retried on the next fetch.
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

		// Classify the whole row before mutating it: a legacy row may hold >1
		// message, and deleting on the first expiry would orphan live siblings.
		survivors := make([]msgtypes.Message, 0, len(msgs))

		rowDropped := 0
		rowStamped := false

		for _, m := range msgs {
			if !m.QueuedAt.IsZero() && now.Sub(m.QueuedAt) > MaxQueueAge {
				rowDropped++

				continue
			}

			// Stamp legacy zero QueuedAt so MaxQueueAge applies from now on.
			// The stamp must be persisted (see the rewrite below) — otherwise
			// every fetch re-bases the age and the row never expires.
			if m.QueuedAt.IsZero() {
				m.QueuedAt = now
				rowStamped = true
			}

			survivors = append(survivors, m)
		}

		dropped += rowDropped

		// Whole row expired: delete it.
		if len(survivors) == 0 {
			Dequeue(ctx, eDB, row.Key)

			continue
		}

		// Multiple survivors: split into per-message rows so the caller's
		// per-message Dequeue stays crash-safe — with a shared key, the first
		// Dequeue would orphan live siblings. On split failure the original row
		// is left intact and the row is skipped this cycle (retried on the next
		// fetch), losing nothing.
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

		// Single survivor: rewrite in place when expiry or a QueuedAt stamp
		// actually changed the contents.
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

// splitRow rewrites a multi-message row as one row per message, returning the
// new keys in survivor order, and removes the original row. On any failure it
// rolls back the partial split (leaving the original intact) and returns nil
// so the caller skips the row this cycle — no loss, no duplication.
func splitRow(ctx context.Context, eDB *sqlitedb.Edb, queueKey, origKey []byte, survivors []msgtypes.Message) [][]byte {
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

// migrateLegacyQueue splits a pre-redesign aggregate queue row (whole queue
// as one CBOR list stored under the bare queue name) into per-message rows.
// The aggregate row is deleted only after every message has been re-stored,
// so a crash mid-migration duplicates rather than loses; the migration is
// idempotent apart from those duplicates. It reports whether the legacy row
// is gone (absent, undecodable, or fully migrated) so the caller can skip
// future probes; a mid-migration store failure reports not-done and is
// retried on the next fetch.
func migrateLegacyQueue(ctx context.Context, eDB *sqlitedb.Edb, queueKey []byte) bool {
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

		return false
	}

	if len(legacy) == 0 {
		return true
	}

	logger.Info().Msgf("Migrating %v messages from legacy queue %v to per-message rows", len(legacy), string(queueKey))

	for _, m := range legacy {
		if err := StoreFailedMsgs(ctx, eDB, queueKey, m); err != nil {
			logger.Error().Msgf("%v: %v", ErrQueueing, err)

			// Keep the aggregate row so nothing is lost; retry next fetch.
			return false
		}
	}

	if err := eDB.Delete(ctx, queueKey); err != nil {
		// The rows are already migrated and the leftover aggregate is
		// invisible to the prefix scan — re-migrating would duplicate, so a
		// failed delete still counts as done.
		logger.Error().Msgf("%v: %v", ErrQueueing, err)
	}

	return true
}
