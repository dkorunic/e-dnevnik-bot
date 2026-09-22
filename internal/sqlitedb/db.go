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

// Edb is the dedup and queue store.
type Edb struct {
	db         *sql.DB
	isExisting bool // already created/initialized db
}

// New opens the database, recording whether it already held entries.
func New(ctx context.Context, filePath string) (*Edb, error) {
	if filePath == "" {
		filePath = DefaultDBPath
	}

	if !strings.HasSuffix(filePath, ".sqlite") {
		filePath += ".sqlite"
	}

	isExisting := dbExists(filePath)

	logger.Debug().Msgf("Opening database: %v", filePath)

	// Encoded so a '?', '#' or '%' cannot corrupt the pragma query.
	sqlitePath := "file:" + sqliteURIEscape(filePath) + DefaultDBOptions

	db, err := sql.Open("sqlite", sqlitePath)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSqliteOpen, err)
	}

	// WAL gives concurrent readers one writer; busy_timeout covers contention.
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

	// A present-but-empty table must read as fresh, so seeding stays silent.
	// This subsumes the stat/open TOCTOU, truncated or purged files, and
	// crashed first runs.
	hasRows, err := edb.hasAnyRow(ctx)
	if err != nil {
		logger.Error().Msgf("Unable to probe database for existing entries, falling back to file presence: %v", err)
	} else {
		edb.isExisting = hasRows
	}

	edb.cleanup(ctx)

	return edb, nil
}

// Close releases the connection pool.
func (db *Edb) Close() error {
	logger.Debug().Msg("Closing database")

	return db.db.Close()
}

// CheckAndFlagTTL reports whether the key was already seen, flagging it with a
// 1+ year TTL if not.
//
// BEGIN IMMEDIATE on a dedicated conn: BeginTx's BEGIN DEFERRED would let the
// SELECT race, and two callers could both flag the same key.
//
// A missing current-format key falls back to the legacy separator-less hash. A
// live legacy hit counts as seen and is re-flagged under the current key, so the
// old row ages out and an upgrade does not re-alert every historical event.
func (db *Edb) CheckAndFlagTTL(ctx context.Context, bucket, subBucket string, target []string) (bool, error) {
	key := hashContent(bucket, subBucket, target)

	now := time.Now()

	conn, err := db.db.Conn(ctx)
	if err != nil {
		return false, err
	}
	defer conn.Close()

	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return false, err
	}

	committed := false

	defer func() { //nolint:contextcheck // detaching is the point, see below
		if !committed {
			// Fresh ctx: rollback must run even once the caller's is cancelled.
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	found, err := keyLive(ctx, conn, key, now)
	if err != nil {
		return false, err
	}

	migrated := false

	if !found {
		// Flagged by a pre-separator release.
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

	// Absent or expired: flag under the current key. A legacy hit migrates
	// forward with a fresh TTL, and the old row expires on its own.
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

// keyLive reports whether key exists and is within its TTL. An expired row reads
// as absent, so stale events re-fire after ~1 year.
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

// Existing reports whether the database held entries at open time. An empty one
// reads as fresh, so the first run seeds silently rather than flooding.
func (db *Edb) Existing() bool {
	return db.isExisting
}

// FetchAndStore atomically applies f to key's value, an empty result deleting
// the row. BEGIN IMMEDIATE on a dedicated conn: BeginTx's BEGIN DEFERRED would
// leave the SELECT racing other writers, and concurrent callers could lose each
// other's updates. Queue rows carry no TTL.
func (db *Edb) FetchAndStore(ctx context.Context, key []byte, f func(old []byte) ([]byte, error)) error {
	conn, err := db.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}

	committed := false

	defer func() { //nolint:contextcheck // detaching is the point, see below
		if !committed {
			// Fresh ctx: rollback must run even once the caller's is cancelled.
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

	// Unchanged needs no write; an expired row always writes, to refresh expiry.
	if !wasExpired && bytes.Equal(val, newVal) {
		if _, err = conn.ExecContext(ctx, "COMMIT"); err != nil {
			return err
		}

		committed = true

		return nil
	}

	if len(newVal) == 0 {
		// Drained: delete rather than leave a NULL-TTL zombie.
		_, err = conn.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", key)
	} else {
		// Queue rows never expire.
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

// Put writes a row with no TTL, replacing any existing one. Queue rows use this:
// their ageing is MaxQueueAge's job at fetch time, not the TTL sweep's.
func (db *Edb) Put(ctx context.Context, key, value []byte) error {
	_, err := db.db.ExecContext(ctx,
		"INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, NULL)", key, value)

	return err
}

// Delete removes a key; an absent one is a no-op, not an error.
func (db *Edb) Delete(ctx context.Context, key []byte) error {
	_, err := db.db.ExecContext(ctx, "DELETE FROM kv WHERE key = ?", key)

	return err
}

// KV is a single key/value row returned by ScanPrefix.
type KV struct {
	Key   []byte
	Value []byte
}

// ScanPrefix returns every row under prefix, key-ascending. The upper bound
// increments the last prefix byte; an all-0xFF tail falls back to a full ordered
// scan filtered client-side, which our 0x00-terminated queue prefixes never
// hit.
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

// prefixUpperBound returns the smallest key above every key under prefix, or nil
// when none exists.
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

// hasAnyRow reports whether the table holds any row.
func (db *Edb) hasAnyRow(ctx context.Context) (bool, error) {
	var one int

	err := db.db.QueryRowContext(ctx, "SELECT 1 FROM kv LIMIT 1").Scan(&one)
	if err == nil {
		return true, nil
	}

	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}

	return false, err
}

// Caps per-pass deletes so concurrent queue writes don't stall on the lock.
const cleanupBatchSize = 10000

// cleanup removes expired keys. modernc.org/sqlite is built without
// SQLITE_ENABLE_UPDATE_DELETE_LIMIT, so `DELETE ... LIMIT` will not parse; the
// subquery form bounds the batch the same way.
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
