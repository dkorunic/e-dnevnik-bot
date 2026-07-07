// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// openDB opens the dedup/queue database at file, fatal on failure.
func openDB(ctx context.Context, file string) *sqlitedb.Edb {
	eDB, err := sqlitedb.New(ctx, file)
	if err != nil {
		logger.Fatal().Msgf("Unable to open application database: %v", err)
	}

	return eDB
}

// closeDB closes the application database.
func closeDB(eDB *sqlitedb.Edb) {
	if err := eDB.Close(); err != nil {
		logger.Fatal().Msgf("Unable to close application database: %v", err)
	}
}
