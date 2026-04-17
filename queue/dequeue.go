// @license
// Copyright (C) 2025  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package queue

import (
	"context"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/encdec"
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

	// fetch failed messages list, store empty list
	err := eDB.FetchAndStore(ctx, queueKey, func(old []byte) ([]byte, error) {
		var decErr error

		failedList, decErr = encdec.DecodeMsgs(old)
		if decErr != nil {
			logger.Warn().Msgf("Failed to decode queue %q, returning empty list: %v", queueKeyStr, decErr)

			failedList = []msgtypes.Message{}
		}

		return encdec.EncodeMsgs([]msgtypes.Message{})
	})
	if err != nil {
		logger.Error().Msgf("Error managing failed messages list for queue %v: %v", queueKeyStr, err)

		return []msgtypes.Message{}
	}

	// Drop entries older than MaxQueueAge to prevent unbounded retries when a
	// messenger is persistently broken. Entries with a zero QueuedAt predate
	// this field (legacy/pre-upgrade queue) and are kept so they get at least
	// one more chance; they will be stamped on next re-queue.
	now := time.Now()
	kept := failedList[:0]
	dropped := 0

	for _, m := range failedList {
		if !m.QueuedAt.IsZero() && now.Sub(m.QueuedAt) > MaxQueueAge {
			dropped++

			continue
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
