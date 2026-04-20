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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/logger"
	_ "modernc.org/sqlite" // register pure-Go sqlite database/sql driver
)

const (
	DefaultDBPath    = ".e-dnevnik.db"
	DefaultEntryTTL  = time.Hour * 9000 // a bit more than 1 year TTL
	DefaultDBOptions = "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(8000)"
)

var (
	ErrSqliteOpen        = errors.New("could not open Sqlite database")
	ErrSqliteCreateTable = errors.New("could not create table")
	ErrDeleteBadgerDB    = errors.New("could not remove old BadgerDB directory, please delete manually")
	importOnce           sync.Once // BadgerDB migration must run at most once per process lifetime.
)

// Edb holds e-dnevnik structure including sql.DB struct.
type Edb struct {
	db         *sql.DB
	isExisting bool // already created/initialized db
}

// New opens a new database, flagging if the database already preexisting.
func New(ctx context.Context, filePath string) (*Edb, error) {
	if filePath == "" {
		filePath = DefaultDBPath
	}

	origFilePath := filePath

	if !strings.HasSuffix(filePath, ".sqlite") {
		filePath += ".sqlite"
	}

	isExisting := dbExists(filePath)

	logger.Debug().Msgf("Opening database: %v", filePath)

	sqlitePath := "file:" + filePath + DefaultDBOptions

	db, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSqliteOpen, err)
	}

	// WAL allows concurrent readers alongside one writer; busy_timeout handles rare lock contention.
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)

	query := `
	CREATE TABLE IF NOT EXISTS kv (
		key BLOB PRIMARY KEY,
		value BLOB,
		expires_at INTEGER
	);
	CREATE INDEX IF NOT EXISTS idx_expires_at ON kv(expires_at);
	`
	if _, err = db.ExecContext(ctx, query); err != nil {
		_ = db.Close()

		return nil, fmt.Errorf("%w: %w", ErrSqliteCreateTable, err)
	}

	edb := &Edb{db: db, isExisting: isExisting}

	var importErr error

	importOnce.Do(func() {
		// One-shot, destructive BadgerDB migration; runs at most once per process.
		edb, importErr = badgerDB2Sqlite(ctx, origFilePath, edb)
	})
	if importErr != nil {
		_ = edb.Close()

		return nil, importErr
	}

	edb.cleanup(ctx)

	return edb, nil
}

// badgerDB2Sqlite checks if the given path is an old BadgerDB directory
// and if so, imports all data into the given Edb and removes the old
// BadgerDB directory.
// Returns the Edb with imported data and an error if any occurred.
func badgerDB2Sqlite(ctx context.Context, origFilePath string, edb *Edb) (*Edb, error) {
	fid, errDir := os.Stat(origFilePath)                                 // check if it is BadgerDB directory
	fim, errManifest := os.Stat(filepath.Join(origFilePath, "MANIFEST")) // check if there is MANIFEST file inside

	if errDir == nil && fid.IsDir() && errManifest == nil && fim.Mode().IsRegular() {
		if err := edb.ImportFromBadger(ctx, origFilePath); err != nil {
			_ = edb.Close()

			return nil, err
		}

		// Imported rows count as pre-existing data to suppress first-run seeding.
		edb.isExisting = true

		logger.Info().Msgf("Removing BadgerDB directory post-import at %v", origFilePath)

		if err := os.RemoveAll(origFilePath); err != nil {
			return edb, fmt.Errorf("%w: %w", ErrDeleteBadgerDB, err)
		}
	}

	return edb, nil
}

// Close closes database.
func (db *Edb) Close() error {
	logger.Debug().Msg("Closing database")

	return db.db.Close()
}

// CheckAndFlagTTL checks if a key already exists in the database and marks it with a flag
// if it doesn't exist. The flag is set with a TTL of 1+ year.
//
// The key is created by hashing a concatenation of the bucket, subBucket and target
// strings using SHA-256.
//
// If the key already exists, the function returns (true, nil). If the key doesn't
// exist, the function marks the key and returns (false, nil) on success or
// (false, error) on error.
//
// The check-then-insert pair is wrapped in a SQLite BEGIN IMMEDIATE transaction
// so that two concurrent callers cannot both observe "not found" and each
// insert the same key. A dedicated *sql.Conn is used because database/sql's
// BeginTx() issues BEGIN DEFERRED, which acquires the write lock lazily on
// first write and leaves the SELECT above racing with other writers.
func (db *Edb) CheckAndFlagTTL(ctx context.Context, bucket, subBucket string, target []string) (bool, error) {
	key := hashContent(bucket, subBucket, target)

	now := time.Now()

	conn, err := db.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close() //nolint:errcheck // releasing the conn back to the pool

	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}

	committed := false

	defer func() {
		if !committed {
			// Fresh context ensures rollback runs even if caller's ctx is cancelled.
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var expiresAt sql.NullInt64

	err = conn.QueryRowContext(ctx, "SELECT expires_at FROM kv WHERE key = ?", key).Scan(&expiresAt)
	switch {
	case err == nil:
		// Still within TTL: report as already-flagged.
		if !expiresAt.Valid || expiresAt.Int64 >= now.Unix() {
			if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
				return false, err
			}

			committed = true

			return true, nil
		}
		// Expired row: re-insert below so stale events re-fire after ~1 year.
	case errors.Is(err, sql.ErrNoRows):
	default:
		return false, err
	}

	expiry := now.Add(DefaultEntryTTL).Unix()
	if _, err = conn.ExecContext(ctx, "INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, ?)",
		key, []byte(""), expiry); err != nil {
		return false, err
	}

	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}

	committed = true

	return false, nil
}

// Existing returns if the database was freshly initialized.
func (db *Edb) Existing() bool {
	return db.isExisting
}

// FetchAndStore fetches a value by key, applies a given function to the value
// and stores the result.
//
// It does the following steps:
//
// 1. Finds the key in the database.
// 2. Copies the associated value.
// 3. Calls the given function with the copied value as argument and stores the result.
// 4. Stores the result in the database with the same key and a TTL of 1+ year.
//
// The fetch/modify/store sequence is wrapped in a SQLite BEGIN IMMEDIATE
// transaction so two concurrent callers (for the same queue key) cannot each
// read the same snapshot, apply their own transformation, and then race to
// overwrite the other's result — which would silently drop queue entries. A
// dedicated *sql.Conn is used because database/sql's BeginTx() issues BEGIN
// DEFERRED, which acquires the write lock lazily on first write and leaves the
// initial SELECT racing with other writers.
//
// If any of the steps fail, it will return an error.
func (db *Edb) FetchAndStore(ctx context.Context, key []byte, f func(old []byte) ([]byte, error)) error {
	conn, err := db.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close() //nolint:errcheck // releasing the conn back to the pool

	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}

	committed := false

	defer func() {
		if !committed {
			// Fresh context ensures rollback runs even if caller's ctx is cancelled.
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var val []byte

	var expiresAt sql.NullInt64

	err = conn.QueryRowContext(ctx, "SELECT value, expires_at FROM kv WHERE key = ?", key).Scan(&val, &expiresAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	wasExpired := false

	if err == nil {
		if expiresAt.Valid && expiresAt.Int64 < time.Now().Unix() {
			val = nil
			wasExpired = true // force write to refresh expiry
		}
	} else {
		val = nil
	}

	newVal, err := f(val)
	if err != nil {
		return err
	}

	// Skip write when unchanged; always write if expired to refresh expiry.
	if !wasExpired && bytes.Equal(val, newVal) {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}

		committed = true

		return nil
	}

	if len(newVal) == 0 {
		// Drained queue: delete the row so cleanup() isn't left with a NULL-TTL zombie.
		_, err = conn.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", key)
	} else {
		// Queue rows use NULL expires_at (no TTL).
		_, err = conn.ExecContext(ctx, "INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, NULL)", key, newVal)
	}

	if err != nil {
		return err
	}

	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}

	committed = true

	return nil
}

// cleanup removes expired keys.
func (db *Edb) cleanup(ctx context.Context) {
	_, err := db.db.ExecContext(ctx, "DELETE FROM kv WHERE expires_at IS NOT NULL AND expires_at < ?", time.Now().Unix())
	if err != nil {
		logger.Error().Msgf("Failed to cleanup expired keys: %v", err)
	}
}
