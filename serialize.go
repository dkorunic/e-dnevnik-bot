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

package main

import (
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/encdec"
	"github.com/dkorunic/e-dnevnik-bot/logger"
)

// fetchAndSendFailedMsg fetches a list of failed messages from the database using the
// queueKey, decodes the GOB-encoded value, and sends each message to the given
// channel. If there is an error, the function returns immediately.
func fetchAndSendFailedMsg(eDB *db.Edb, ch chan<- interface{}, queueKey []byte) {
	// fetch and delete failed messages list
	val, err := eDB.FetchAndDelete(queueKey)
	if err != nil {
		return
	}

	failedList, err := encdec.DecodeMsgs(val)
	if err != nil {
		return
	}

	failedCount := len(failedList)
	if failedCount > 0 {
		logger.Info().Msgf("Found %v failed messages in %v, trying to resend", failedCount, queueKey)
	}

	// send to channel for processing
	for _, u := range failedList {
		ch <- u
	}
}
