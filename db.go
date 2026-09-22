// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// openDB opens the dedup and queue database.
//
// Not Fatal: this runs once per poll cycle, so exiting would end a daemon over
// something transient — a locked file, a briefly unavailable mount — and
// os.Exit runs no defers, taking the pprof flush and main's shutdown with it.
// The caller skips the cycle instead.
func openDB(ctx context.Context, file string) (*sqlitedb.Edb, error) {
	eDB, err := sqlitedb.New(ctx, file)
	if err != nil {
		return nil, err
	}

	return eDB, nil
}

// dbCloser is a seam: database/sql closes its pool idempotently and never fails
// twice, so a failing close is only reachable through a stub.
type dbCloser interface {
	Close() error
}

// closeDB closes the database, latching the run as failed on error.
//
// Not Fatal, as in msgDedup: every write has committed and the next cycle opens
// a fresh handle, so a close error is no reason to kill a daemon — and os.Exit
// would skip the pprof flush and main's shutdown.
func closeDB(eDB dbCloser) {
	if err := eDB.Close(); err != nil {
		logger.Error().Msgf("Unable to close application database: %v", err)
		exitWithError.Store(true)
	}
}
