// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"strconv"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/rs/zerolog"
)

// initLog sets the global log level to the level specified by the -v
// command-line flag or the LOG_LEVEL environment variable. If the
// -v flag is specified, the log level is set to DebugLevel. If LOG_LEVEL
// environment variable is set, the log level is set to the value of the
// variable. If neither are specified, the log level is set to InfoLevel.
//
// If the -l command-line flag is specified, initLog enables slow colored
// console logging. This is useful for debugging.
//
//nolint:nestif
func initLog() {
	logLevel := zerolog.InfoLevel
	if *debug {
		logLevel = zerolog.DebugLevel
	} else if v, ok := os.LookupEnv("LOG_LEVEL"); ok {
		// Range-check avoids silent reinterpret of out-of-range casts.
		if l, err := strconv.Atoi(v); err == nil &&
			l >= int(zerolog.TraceLevel) && l <= int(zerolog.Disabled) {
			logLevel = zerolog.Level(l)
		}
	}

	zerolog.SetGlobalLevel(logLevel)

	if *colorLogs {
		logger.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}).
			Level(logLevel).
			With().
			Timestamp().
			Caller().
			Logger()
	}
}
