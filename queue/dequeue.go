// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"context"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/codec"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
)

// FetchFailedMsgs fetches failed messages from a persistent queue identified by key
// so they can be resent. The function returns the list of failed messages.
//
// The function assumes the database and the key are valid. If the key doesn't exist, it will be created.
//
// If any of the operations fail, the function will log an error and return an empty list.
func FetchFailedMsgs(ctx context.Context, eDB *sqlitedb.Edb, queueKey []byte) []msgtypes.Message {
	var failedList []msgtypes.Message

	queueKeyStr := string(queueKey)

	// Read-and-drain atomically: returning empty clears the queue.
	err := eDB.FetchAndStore(ctx, queueKey, func(old []byte) ([]byte, error) {
		var decErr error

		failedList, decErr = codec.DecodeMsgs(old)
		if decErr != nil {
			logger.Warn().Msgf("Failed to decode queue %q, returning empty list: %v", queueKeyStr, decErr)

			failedList = []msgtypes.Message{}
		}

		return codec.EncodeMsgs([]msgtypes.Message{})
	})
	if err != nil {
		logger.Error().Msgf("Error managing failed messages list for queue %v: %v", queueKeyStr, err)

		return []msgtypes.Message{}
	}

	// Stamp legacy zero QueuedAt so MaxQueueAge applies on next cycle.
	now := time.Now()
	// Fresh backing array; aliasing failedList[:0] would be a fragile invariant.
	kept := make([]msgtypes.Message, 0, len(failedList))
	dropped := 0

	for _, m := range failedList {
		if !m.QueuedAt.IsZero() && now.Sub(m.QueuedAt) > MaxQueueAge {
			dropped++

			continue
		}

		if m.QueuedAt.IsZero() {
			m.QueuedAt = now
		}

		kept = append(kept, m)
	}

	failedList = kept

	if dropped > 0 {
		logger.Warn().Msgf("Dropped %v messages older than %v from queue %v", dropped, MaxQueueAge, queueKeyStr)
	}

	failedCount := len(failedList)
	if failedCount > 0 {
		logger.Info().Msgf("Found %v failed messages in queue %v, trying to resend", failedCount, queueKeyStr)
	}

	return failedList
}
