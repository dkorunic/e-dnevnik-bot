// @license
// Copyright (C) 2025  Dinko Korunic
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

package main

import (
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/logger"
)

// openDB opens application database and returns handle to it.
//
// If there is a problem while opening the database, it will log the error and
// exit the program.
//
// The database is opened with default settings, which are:
//   - setting value log file size to 1 MB (default is 1 GB, which is too much for
//     this application)
//   - setting discard ratio to 0.5 (recommended by Badger)
//
// The application database is stored in a file in the current working directory
// with the name given by the `dbFile` flag.
func openDB(file string) *db.Edb {
	eDB, err := db.New(file)
	if err != nil {
		logger.Fatal().Msgf("Unable to open application database: %v", err)
	}

	return eDB
}

// closeDB closes the application database.
func closeDB(eDB *db.Edb) {
	if err := eDB.Close(); err != nil {
		logger.Fatal().Msgf("Unable to close application database: %v", err)
	}
}
