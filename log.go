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

// initLog sets the global level from -v or LOG_LEVEL, defaulting to Info, and
// switches to console output when -l is set.
func initLog() {
	logLevel := zerolog.InfoLevel
	if *debug {
		logLevel = zerolog.DebugLevel
	} else if v, ok := os.LookupEnv("LOG_LEVEL"); ok {
		// Range-checked: an out-of-range cast would silently reinterpret.
		if l, err := strconv.Atoi(v); err == nil &&
			l >= int(zerolog.TraceLevel) && l <= int(zerolog.Disabled) {
			logLevel = zerolog.Level(l)
		}
	}

	zerolog.SetGlobalLevel(logLevel)

	if *colorLogs {
		// NO_COLOR (https://no-color.org) wins over -l.
		noColor := os.Getenv("NO_COLOR") != ""

		logger.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339, NoColor: noColor}).
			Level(logLevel).
			With().
			Timestamp().
			Caller().
			Logger()
	}
}
