// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package sqlitedb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

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
		badgerPath = ".e-dnevnik.db"
	}

	if !dbExists(badgerPath) {
		return fmt.Errorf("%w: %s", ErrBadgerDBNotFound, badgerPath)
	}

	logger.Info().Msgf("Importing data from BadgerDB at %s", badgerPath)

	opts := badger.DefaultOptions(badgerPath).
		WithLogger(nil)

	// 32-bit hosts get a smaller value log to avoid mmap pressure.
	if strconv.IntSize == 32 {
		logger.Info().Msg("Detected 32-bit environment, tuning DB for lower memory usage")

		opts.ValueLogFileSize = 1 << 20 //nolint:mnd
	}

	bdb, err := badger.Open(opts)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrBadgerDBOpen, err)
	}
	defer bdb.Close()

	var count, skipped int

	// Snapshot once so the per-row check is consistent across the import.
	nowUnix := time.Now().Unix()

	// Single transaction keeps bulk insert fast.
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
				expiresAt := item.ExpiresAt()

				var expiry sql.NullInt64

				if expiresAt > 0 && expiresAt <= math.MaxInt64 {
					// Skip rows already past their TTL: cleanup() would delete
					// them moments later, so the events they de-duplicate would
					// re-alert on the next scrape after migration.
					if int64(expiresAt) < nowUnix {
						skipped++

						return nil
					}

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

	if skipped > 0 {
		logger.Info().Msgf("Skipped %d already-expired BadgerDB rows during import", skipped)
	}

	logger.Info().Msgf("Successfully imported %d items from BadgerDB", count)

	return nil
}
