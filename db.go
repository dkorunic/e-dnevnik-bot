// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// openDB opens application database and returns handle to it.
//
// If there is a problem while opening the database, it will log the error and
// exit the program.
//
// The application database is stored in a file in the current working directory
// with the name given by the `dbFile` flag.
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
