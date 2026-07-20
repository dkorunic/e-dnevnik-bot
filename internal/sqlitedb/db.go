// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package sqlitedb

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
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

	if !strings.HasSuffix(filePath, ".sqlite") {
		filePath += ".sqlite"
	}

	isExisting := dbExists(filePath)

	logger.Debug().Msgf("Opening database: %v", filePath)

	// Encode the path so a '?'/'#'/'%' in it can't corrupt the pragma query.
	sqlitePath := "file:" + sqliteURIEscape(filePath) + DefaultDBOptions

	db, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSqliteOpen, err)
	}

	// WAL: concurrent readers with one writer; busy_timeout handles contention.
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

	edb.cleanup(ctx)

	return edb, nil
}

// Close closes database.
func (db *Edb) Close() error {
	logger.Debug().Msg("Closing database")

	return db.db.Close()
}

// CheckAndFlagTTL reports whether (bucket, subBucket, target) was already seen,
// flagging it with a 1+ year TTL if not. Returns true for an existing live key.
//
// Runs under BEGIN IMMEDIATE on a dedicated conn (BeginTx's BEGIN DEFERRED
// would race the SELECT) so two callers can't both flag the same key.
//
// A missing current-format key falls back to the legacy separator-less hash:
// a live legacy hit counts as seen and is re-flagged under the current key,
// letting the old row age out. This dual lookup stops an upgrade from
// re-alerting every historical event.
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
			// Fresh ctx: rollback must run even if caller's ctx is cancelled.
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	found, err := keyLive(ctx, conn, key, now)
	if err != nil {
		return false, err
	}

	migrated := false

	if !found {
		// Fallback: row flagged by a pre-separator release.
		migrated, err = keyLive(ctx, conn, hashContentLegacy(bucket, subBucket, target), now)
		if err != nil {
			return false, err
		}
	}

	if found {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return false, err
		}

		committed = true

		return true, nil
	}

	// Not found (or expired): flag under the current-format key. For a legacy
	// hit this migrates the row forward with a fresh TTL; the legacy row is
	// left to expire on its own.
	expiry := now.Add(DefaultEntryTTL).Unix()
	if _, err = conn.ExecContext(ctx, "INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, ?)",
		key, []byte(""), expiry); err != nil {
		return false, err
	}

	if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
		return false, err
	}

	committed = true

	return migrated, nil
}

// keyLive reports whether key exists and is still within its TTL. Expired
// rows are treated as absent so stale events re-fire after ~1 year.
func keyLive(ctx context.Context, conn *sql.Conn, key []byte, now time.Time) (bool, error) {
	var expiresAt sql.NullInt64

	err := conn.QueryRowContext(ctx, "SELECT expires_at FROM kv WHERE key = ?", key).Scan(&expiresAt)
	switch {
	case err == nil:
		return !expiresAt.Valid || expiresAt.Int64 >= now.Unix(), nil
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	default:
		return false, err
	}
}

// Existing returns if the database was freshly initialized.
func (db *Edb) Existing() bool {
	return db.isExisting
}

// FetchAndStore atomically reads key, passes the value to f, and writes f's
// result back (empty result deletes the row). The read-modify-write runs under
// BEGIN IMMEDIATE on a dedicated conn — BeginTx's BEGIN DEFERRED would leave
// the SELECT racing other writers — so concurrent callers on the same key
// cannot lose each other's updates. Queue rows carry no TTL.
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
			// Fresh ctx: rollback must run even if caller's ctx is cancelled.
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
			wasExpired = true
		}
	} else {
		val = nil
	}

	newVal, err := f(val)
	if err != nil {
		return err
	}

	// Skip write when unchanged; expired rows always write to refresh expiry.
	if !wasExpired && bytes.Equal(val, newVal) {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}

		committed = true

		return nil
	}

	if len(newVal) == 0 {
		// Drained queue: delete row to avoid a NULL-TTL zombie.
		_, err = conn.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", key)
	} else {
		// Queue rows: NULL expires_at (no TTL).
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

// Put stores a key/value pair with no TTL (expires_at NULL), replacing any
// existing row. Queue rows use this: they must never expire via the TTL
// cleanup pass — queue aging is handled at fetch time by MaxQueueAge.
func (db *Edb) Put(ctx context.Context, key, value []byte) error {
	_, err := db.db.ExecContext(ctx,
		"INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, NULL)", key, value)

	return err
}

// Delete removes a key. Deleting a non-existent key is a no-op, not an error.
func (db *Edb) Delete(ctx context.Context, key []byte) error {
	_, err := db.db.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", key)

	return err
}

// KV is a single key/value row returned by ScanPrefix.
type KV struct {
	Key   []byte
	Value []byte
}

// ScanPrefix returns all rows whose key starts with prefix, ordered by key
// ascending. The upper bound is computed by incrementing the last prefix
// byte; a prefix ending in a run of 0xFF bytes falls back to a full ordered
// scan filtered client-side (never the case for our queue prefixes, which
// end in a 0x00 separator).
func (db *Edb) ScanPrefix(ctx context.Context, prefix []byte) ([]KV, error) {
	upper := prefixUpperBound(prefix)

	var (
		rows *sql.Rows
		err  error
	)

	if upper != nil {
		rows, err = db.db.QueryContext(ctx,
			"SELECT key, value FROM kv WHERE key >= ? AND key < ? ORDER BY key ASC", prefix, upper)
	} else {
		rows, err = db.db.QueryContext(ctx,
			"SELECT key, value FROM kv WHERE key >= ? ORDER BY key ASC", prefix)
	}

	if err != nil {
		return nil, err
	}

	defer rows.Close() // read-only cursor cleanup

	var out []KV

	for rows.Next() {
		var kv KV
		if err := rows.Scan(&kv.Key, &kv.Value); err != nil {
			return nil, err
		}

		if !bytes.HasPrefix(kv.Key, prefix) {
			continue
		}

		out = append(out, kv)
	}

	return out, rows.Err()
}

// prefixUpperBound returns the smallest key strictly greater than every key
// with the given prefix, or nil if no such bound exists (all-0xFF prefix).
func prefixUpperBound(prefix []byte) []byte {
	upper := bytes.Clone(prefix)
	for i := len(upper) - 1; i >= 0; i-- {
		if upper[i] < 0xFF {
			upper[i]++

			return upper[:i+1]
		}
	}

	return nil
}

// cleanupBatchSize caps per-pass deletes so concurrent queue writes don't stall on the writer lock.
const cleanupBatchSize = 10000

// cleanup removes expired keys. modernc.org/sqlite is built without
// SQLITE_ENABLE_UPDATE_DELETE_LIMIT, so `DELETE ... LIMIT` is not a valid
// statement. The subquery-with-LIMIT form is portable and achieves the same
// batch-size bound.
func (db *Edb) cleanup(ctx context.Context) {
	_, err := db.db.ExecContext(ctx,
		`DELETE FROM kv WHERE key IN (
			SELECT key FROM kv
			WHERE expires_at IS NOT NULL AND expires_at < ?
			LIMIT ?
		)`,
		time.Now().Unix(), cleanupBatchSize)
	if err != nil {
		logger.Error().Msgf("Failed to cleanup expired keys: %v", err)
	}
}
