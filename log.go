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
	"os"
	"strconv"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/reiver/go-cast"
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
func initLog() {
	// set global log level
	logLevel := zerolog.InfoLevel
	if *debug {
		logLevel = zerolog.DebugLevel
	} else {
		if v, ok := os.LookupEnv("LOG_LEVEL"); ok {
			if l, err := strconv.Atoi(v); err == nil {
				if l8, err := cast.Int8(l); err == nil {
					logLevel = zerolog.Level(l8)
				}
			}
		}
	}

	zerolog.SetGlobalLevel(logLevel)

	// enable slow colored console logging
	if *colorLogs {
		logger.Logger = zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.RFC3339}).
			Level(logLevel).
			With().
			Timestamp().
			Caller().
			Logger()
	}
}
