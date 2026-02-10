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
	"errors"

	"github.com/dkorunic/e-dnevnik-bot/encdec"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
)

var ErrQueueing = errors.New("problem with persistent queue")

// StoreFailedMsgs stores a message in a persistent queue identified by key.
// The message is appended to any existing messages in the queue, and the queue
// is stored in the database using the given key.
//
// The function assumes the database and the key are valid. If the key doesn't
// exist, it will be created.
//
// If any of the operations fail, the function returns an error.
func StoreFailedMsgs(eDB *sqlitedb.Edb, key []byte, g msgtypes.Message) error {
	return eDB.FetchAndStore(key, func(old []byte) ([]byte, error) {
		msgs, _ := encdec.DecodeMsgs(old)
		msgs = append(msgs, g)

		return encdec.EncodeMsgs(msgs)
	})
}
