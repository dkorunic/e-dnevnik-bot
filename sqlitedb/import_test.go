// @license
// Copyright (C) 2026  Dinko Korunic
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

package sqlitedb

import (
	"bytes"
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/dgraph-io/badger/v4"
)

func TestImportFromBadger(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	badgerDir := filepath.Join(tmp, "badger")

	bdb, err := badger.Open(badger.DefaultOptions(badgerDir).WithLogger(nil))
	if err != nil {
		t.Fatalf("Failed to open badger: %v", err)
	}

	err = bdb.Update(func(txn *badger.Txn) error {
		if err := txn.Set([]byte("key1"), []byte("val1")); err != nil {
			return err
		}

		e := badger.NewEntry([]byte("key2"), []byte("val2")).WithTTL(time.Hour)
		if err := txn.SetEntry(e); err != nil {
			return err
		}
		return nil
	})
	bdb.Close() // Close so import can reopen exclusively.
	if err != nil {
		t.Fatalf("Failed to write to badger: %v", err)
	}

	sqlitePath := filepath.Join(tmp, "sqlite.db")

	sdb, err := New(context.Background(), sqlitePath)
	if err != nil {
		t.Fatalf("Failed to open sqlite: %v", err)
	}
	defer sdb.Close()

	if err := sdb.ImportFromBadger(context.Background(), badgerDir); err != nil {
		t.Fatalf("ImportFromBadger failed: %v", err)
	}

	var val []byte

	err = sdb.db.QueryRow("SELECT value FROM kv WHERE key = ?", []byte("key1")).Scan(&val)
	if err != nil {
		t.Errorf("Failed to get key1: %v", err)
	}
	if !bytes.Equal(val, []byte("val1")) {
		t.Errorf("key1: got %s, want val1", val)
	}

	var exp sql.NullInt64
	err = sdb.db.QueryRow("SELECT value, expires_at FROM kv WHERE key = ?", []byte("key2")).Scan(&val, &exp)
	if err != nil {
		t.Errorf("Failed to get key2: %v", err)
	}
	if !bytes.Equal(val, []byte("val2")) {
		t.Errorf("key2: got %s, want val2", val)
	}
	if !exp.Valid || exp.Int64 == 0 {
		t.Errorf("key2: expected expiration, got none")
	}
	if exp.Int64 < time.Now().Unix() {
		t.Errorf("key2: expired already?")
	}
}
