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
	"os"
	"path"
	"strings"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/logger"
	_ "modernc.org/sqlite" // register pure-Go sqlite database/sql driver
)

const (
	DefaultDBPath = ".e-dnevnik.db"
	DefaultTTL    = time.Hour * 9000 // a bit more than 1 year TTL
)

var (
	ErrSqliteOpen        = errors.New("could not open Sqlite database")
	ErrSqliteCreateTable = errors.New("could not create table")
	ErrDeleteBadgerDB    = errors.New("could not remove old BadgerDB directory, please delete manually")
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

	// Add .sqlite suffix if not present
	if !strings.HasSuffix(filePath, ".sqlite") {
		filePath = strings.Join([]string{filePath, ".sqlite"}, "")
	}

	isExisting := dbExists(filePath)

	logger.Debug().Msgf("Opening database: %v", filePath)

	// Open database
	db, err := sql.Open("sqlite", filePath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSqliteOpen, err)
	}

	// Create table if not exists
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

	// Check if old BadgerDB directory exists and convert data
	edb, err = badgerDB2Sqlite(ctx, origFilePath, edb)
	if err != nil {
		return edb, err
	}

	// Perform initial cleanup of expired keys
	edb.cleanup(ctx)

	return edb, nil
}

// badgerDB2Sqlite checks if the given path is an old BadgerDB directory
// and if so, imports all data into the given Edb and removes the old
// BadgerDB directory.
// Returns the Edb with imported data and an error if any occurred.
func badgerDB2Sqlite(ctx context.Context, origFilePath string, edb *Edb) (*Edb, error) {
	fid, err := os.Stat(origFilePath)                         // check if it is BadgerDB directory
	fim, err2 := os.Stat(path.Join(origFilePath, "MANIFEST")) // check if there is MANIFEST file inside

	if err == nil && fid.IsDir() && err2 == nil && fim.Mode().IsRegular() {
		err = edb.ImportFromBadger(ctx, origFilePath)
		if err != nil {
			_ = edb.Close()

			return edb, err
		}

		// New database has been populated with data
		edb.isExisting = true

		logger.Info().Msgf("Removing BadgerDB directory post-import at %v", origFilePath)

		err = os.RemoveAll(origFilePath)
		if err != nil {
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
func (db *Edb) CheckAndFlagTTL(ctx context.Context, bucket, subBucket string, target []string) (bool, error) {
	// SHA256 hash of (bucket, subBucket, []target)
	key := hashContent(bucket, subBucket, target)

	var expiresAt sql.NullInt64

	// Check if key exists
	err := db.db.QueryRowContext(ctx, "SELECT expires_at FROM kv WHERE key = ?", key).Scan(&expiresAt)
	if err == nil {
		// Key found
		// ... Check if expired
		if expiresAt.Valid && expiresAt.Int64 < time.Now().Unix() {
			// Expired, treat as not found (and we will update/overwrite it below)
		} else {
			return true, nil
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		// Error other than not found
		return false, err
	}

	// Key not found or expired. Insert/Update with TTL.
	expiry := time.Now().Add(DefaultTTL).Unix()

	_, err = db.db.ExecContext(ctx, "INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, ?)", key, []byte(""), expiry)
	if err != nil {
		return false, err
	}

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
// If any of the steps fail, it will return an error.
func (db *Edb) FetchAndStore(ctx context.Context, key []byte, f func(old []byte) ([]byte, error)) error {
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck

	var val []byte

	var expiresAt sql.NullInt64

	err = tx.QueryRowContext(ctx, "SELECT value, expires_at FROM kv WHERE key = ?", key).Scan(&val, &expiresAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	// If expired, treat as not found
	if err == nil {
		if expiresAt.Valid && expiresAt.Int64 < time.Now().Unix() {
			val = nil // Treat as not found/empty
		}
	} else {
		val = nil // Not found
	}

	// Call conversion function
	newVal, err := f(val)
	if err != nil {
		return err
	}

	// Store new value with no TTL (expires_at = NULL)
	_, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, NULL)", key, newVal)
	if err != nil {
		return err
	}

	return tx.Commit()
}

// cleanup removes expired keys.
func (db *Edb) cleanup(ctx context.Context) {
	_, err := db.db.ExecContext(ctx, "DELETE FROM kv WHERE expires_at IS NOT NULL AND expires_at < ?", time.Now().Unix())
	if err != nil {
		logger.Error().Msgf("Failed to cleanup expired keys: %v", err)
	}
}
