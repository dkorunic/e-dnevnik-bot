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

package messenger

import (
	"errors"

	"github.com/dgraph-io/badger/v4"
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/encdec"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

var ErrQueueing = errors.New("problem with persistent queue")

// storeFailedMsgs stores a message in a persistent queue identified by key.
// The message is appended to any existing messages in the queue, and the queue
// is stored in the database using the given key.
//
// The function assumes the database and the key are valid. If the key doesn't
// exist, it will be created.
//
// If any of the operations fail, the function returns an error.
func storeFailedMsgs(eDB *db.Edb, key []byte, g msgtypes.Message) error {
	// fetch existing messages from DB with queue key as []byte
	msgsEnc, err := eDB.Fetch(key)
	if err != nil && !errors.Is(err, badger.ErrKeyNotFound) {
		return err
	}

	// decode existing messages (can be empty)
	msgs, err := encdec.DecodeMsgs(msgsEnc)
	if err != nil {
		return err
	}

	// append single message to list
	msgs = append(msgs, g)

	// encode to []byte
	msgsEnc, err = encdec.EncodeMsgs(msgs)
	if err != nil {
		return err
	}

	// store to DB
	return eDB.Store(WhatsAppQueueName, msgsEnc)
}
