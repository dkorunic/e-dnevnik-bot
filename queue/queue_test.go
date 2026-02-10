// @license
// Copyright (C) 2025 Dinko Korunic
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
	"os"
	"reflect"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
)

func TestStoreAndFetchFailedMsgs(t *testing.T) {
	t.Parallel()
	// Create a temporary database for testing.
	tmpdir, err := os.MkdirTemp("", "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	eDB, err := sqlitedb.New(tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	key := []byte("test_queue")
	msg1 := msgtypes.Message{Subject: "Test Subject 1", Descriptions: []string{"Test Body 1"}}
	msg2 := msgtypes.Message{Subject: "Test Subject 2", Descriptions: []string{"Test Body 2"}}

	// Store the first message.
	if err := StoreFailedMsgs(eDB, key, msg1); err != nil {
		t.Fatalf("StoreFailedMsgs failed: %v", err)
	}

	// Store the second message.
	if err := StoreFailedMsgs(eDB, key, msg2); err != nil {
		t.Fatalf("StoreFailedMsgs failed: %v", err)
	}

	// Fetch the messages.
	fetchedMsgs := FetchFailedMsgs(eDB, key)
	expectedMsgs := []msgtypes.Message{msg1, msg2}

	if !reflect.DeepEqual(fetchedMsgs, expectedMsgs) {
		t.Errorf("fetched messages do not match expected messages.\nGot: %v\nWant: %v", fetchedMsgs, expectedMsgs)
	}

	// Fetch again to ensure the queue is empty.
	fetchedMsgs = FetchFailedMsgs(eDB, key)
	if len(fetchedMsgs) != 0 {
		t.Errorf("queue should be empty after fetching, but it's not. Got: %v", fetchedMsgs)
	}
}
