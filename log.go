// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"strconv"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/rs/zerolog"
)

// initLog sets the global log level from -v or LOG_LEVEL (default Info) and
// switches to colorized console output when -l is set.
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
		// NO_COLOR (https://no-color.org) suppresses colour even when -l is set.
		noColor := os.Getenv("NO_COLOR") != ""

		logger.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339, NoColor: noColor}).
			Level(logLevel).
			With().
			Timestamp().
			Caller().
			Logger()
	}
}
