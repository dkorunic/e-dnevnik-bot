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
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"github.com/dgraph-io/badger/v4"
	"github.com/dkorunic/e-dnevnik-bot/logger"
)

var (
	ErrBadgerDBNotFound = errors.New("BadgerDB not found")
	ErrBadgerDBOpen     = errors.New("failed to open BadgerDB")
	ErrSqliteTx         = errors.New("failed to begin sqlite transaction")
	ErrSqlitePrepare    = errors.New("failed to prepare statement")
	ErrSqliteImport     = errors.New("import failed")
	ErrSqliteCommit     = errors.New("failed to commit transaction")
)

// ImportFromBadger imports all data from a BadgerDB database into the current Edb.
func (db *Edb) ImportFromBadger(ctx context.Context, badgerPath string) error {
	if badgerPath == "" {
		badgerPath = ".e-dnevnik.db" // Default from db package
	}

	if !dbExists(badgerPath) {
		return fmt.Errorf("%w: %s", ErrBadgerDBNotFound, badgerPath)
	}

	logger.Info().Msgf("Importing data from BadgerDB at %s", badgerPath)

	// Open BadgerDB
	opts := badger.DefaultOptions(badgerPath).
		WithLogger(nil) // Disable badger logging

	bdb, err := badger.Open(opts)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadgerDBOpen, err)
	}
	defer bdb.Close()

	// Count for progress logging
	var count int

	// Start a transaction in SQLite for faster bulk insert
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSqliteTx, err)
	}
	defer tx.Rollback() //nolint:errcheck

	stmt, err := tx.PrepareContext(ctx, "INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, ?)")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSqlitePrepare, err)
	}
	defer stmt.Close()

	err = bdb.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 100

		it := txn.NewIterator(opts)
		defer it.Close()

		for it.Rewind(); it.Valid(); it.Next() {
			item := it.Item()
			k := item.Key()

			err := item.Value(func(v []byte) error {
				// Copy key and value to ensure they persist outside the closure if needed,
				// though here we use them immediately.
				expiresAt := item.ExpiresAt() // Unix timestamp

				var expiry sql.NullInt64

				// Valid expiry
				if expiresAt > 0 && expiresAt <= math.MaxInt64 {
					expiry.Int64 = int64(expiresAt)
					expiry.Valid = true
				}

				_, err := stmt.ExecContext(ctx, k, v, expiry)
				if err != nil {
					return err
				}

				count++

				return nil
			})
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrSqliteImport, err)
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("%w: %w", ErrSqliteCommit, err)
	}

	logger.Info().Msgf("Successfully imported %d items from BadgerDB", count)

	return nil
}
