// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// openDB opens the dedup/queue database at file.
//
// Not Fatal: this runs once per poll cycle, so exiting would end a daemon over
// a failure that may be transient (a locked file, a briefly unavailable mount)
// — and os.Exit runs no defers, taking the pprof flush and main's deferred
// shutdown with it. The caller skips the cycle instead.
func openDB(ctx context.Context, file string) (*sqlitedb.Edb, error) {
	eDB, err := sqlitedb.New(ctx, file)
	if err != nil {
		return nil, err
	}

	return eDB, nil
}

// dbCloser is the Close half of *sqlitedb.Edb. It exists as a seam: database/sql
// closes its pool idempotently and will not fail twice, so a failing close can
// only be exercised through a stub.
type dbCloser interface {
	Close() error
}

// closeDB closes the application database, flagging the run as failed if the
// close errors.
//
// Not Fatal: every write has already committed by this point and the next cycle
// opens a fresh handle, so a close error is no reason to kill a daemon — and
// os.Exit here would skip the pprof flush and main's deferred shutdown. Matches
// msgDedup, which treats a mid-cycle database failure the same way.
func closeDB(eDB dbCloser) {
	if err := eDB.Close(); err != nil {
		logger.Error().Msgf("Unable to close application database: %v", err)
		exitWithError.Store(true)
	}
}
