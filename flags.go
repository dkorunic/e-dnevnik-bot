// @license
// Copyright (C) 2022  Dinko Korunic
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
	"time"

	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/pborman/getopt/v2"
)

const (
	DefaultConfFile     = ".e-dnevnik.toml" // default configuration filename
	DefaultTickInterval = "1h"              // default (and minimal permitted value) is 1 tick per 1h
	DefaultRetries      = 3                 // default retry attempts
)

var (
	debug, daemon, help, emulation, colorLogs                    *bool
	confFile, dbFile, tickIntervalString, cpuProfile, memProfile *string
	tickInterval                                                 time.Duration
	retries                                                      *uint
)

// init initializes flags configuration.
func init() {
	debug = getopt.BoolLong("verbose", 'v', "enable verbose/debug log level")
	daemon = getopt.BoolLong("daemon", 'd', "enable daemon mode (running as a service)")
	help = getopt.BoolLong("help", '?', "display help")
	emulation = getopt.BoolLong("test", 't', "send a test event (to check if messaging works)")
	confFile = getopt.StringLong("conffile", 'f', DefaultConfFile, "configuration file (in TOML)")
	dbFile = getopt.StringLong("database", 'b', db.DefaultDBPath, "alert database file")
	tickIntervalString = getopt.StringLong("interval", 'i', DefaultTickInterval,
		"interval between polls when in daemon mode")
	retries = getopt.UintLong("retries", 'r', DefaultRetries, "default retry attempts on error")
	cpuProfile = getopt.StringLong("cpuprofile", 'c', "", "CPU profile output file")
	memProfile = getopt.StringLong("memprofile", 'm', "", "memory profile output file")
	colorLogs = getopt.BoolLong("colorlogs", 'l', "enable colorized console logs")
}

// parseFlags parses input arguments and flags.
func parseFlags() {
	getopt.Parse()

	if *help {
		getopt.PrintUsage(os.Stderr)
		os.Exit(0)
	}

	var err error

	tickInterval, err = time.ParseDuration(*tickIntervalString)
	if err != nil {
		logger.Fatal().Msgf("Unable to parse the poll interval %v: %v", tickIntervalString, err)
	}

	if tickInterval < time.Hour {
		logger.Info().Msg("Poll interval is below 1h, so I will default to 1h")

		tickInterval = time.Hour
	}
}
