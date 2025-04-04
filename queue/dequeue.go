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
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/encdec"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

// FetchFailedMsgs fetches failed messages from a persistent queue identified by key
// and attempts to send them again. The function returns the list of failed messages.
//
// The function assumes the database and the key are valid. If the key doesn't exist, it will be created.
//
// If any of the operations fail, the function will log an error and return an empty list.
func FetchFailedMsgs(eDB *db.Edb, queueKey []byte) []msgtypes.Message {
	var failedList []msgtypes.Message

	// fetch failed messages list, store empty list
	err := eDB.FetchAndStore(queueKey, func(old []byte) ([]byte, error) {
		failedList, _ = encdec.DecodeMsgs(old)

		return encdec.EncodeMsgs([]msgtypes.Message{})
	})
	if err != nil {
		logger.Error().Msgf("Error managing failed messages list in database for %v: %v", queueKey, err)

		return []msgtypes.Message{}
	}

	failedCount := len(failedList)
	if failedCount > 0 {
		logger.Info().Msgf("Found %v failed messages in %v, trying to resend", failedCount, queueKey)
	}

	return failedList
}
