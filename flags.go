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
	"fmt"
	"os"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"
)

const (
	DefaultConfFile      = ".e-dnevnik.toml"     // default configuration filename
	DefaultCalendarToken = "calendar_token.json" // default Google Calendar token file
	DefaultTickInterval  = 1 * time.Hour         // default (and minimal permitted value) is 1 tick per 1h
	DefaultRetries       = 3                     // default retry attempts
)

var (
	debug, debugEvents, daemon, help, emulation, colorLogs, version *bool
	confFile, dbFile, cpuProfile, memProfile, calTokFile            *string
	tickInterval                                                    *time.Duration
	retries                                                         *uint
)

// parseFlags parses the command line flags and sets the corresponding variables.
func parseFlags() {
	fs := ff.NewFlagSet("e-dnevnik-bot")

	debug = fs.Bool('v', "verbose", "verbose/debug log level")
	debugEvents = fs.Bool('0', "fulldebug", "log every scraped event (only with verbose mode)")
	daemon = fs.Bool('d', "daemon", "enable daemon mode (running as a service)")
	help = fs.Bool('?', "help", "display help")
	emulation = fs.Bool('t', "test", "send a test event (to check if messaging works)")
	colorLogs = fs.Bool('l', "colorlogs", "enable colorized console logs")
	version = fs.BoolLong("version", "display program version")

	confFile = fs.String('f', "conffile", DefaultConfFile, "configuration file (in TOML)")
	dbFile = fs.String('b', "database", db.DefaultDBPath, "alert database file")
	calTokFile = fs.String('g', "calendartoken", DefaultCalendarToken, "Google Calendar token file")
	cpuProfile = fs.String('c', "cpuprofile", "", "CPU profile output file")
	memProfile = fs.String('m', "memprofile", "", "memory profile output file")

	tickInterval = fs.Duration('i', "interval", DefaultTickInterval, "interval between polls when in daemon mode")

	retries = fs.Uint('r', "retries", DefaultRetries, "number of retry attempts on error")

	var err error

	if err = ff.Parse(fs, os.Args[1:]); err != nil {
		fmt.Printf("%s\n", ffhelp.Flags(fs))
		fmt.Printf("Error: %v\n", err)

		os.Exit(1)
	}

	if *help {
		fmt.Printf("%s\n", ffhelp.Flags(fs))

		os.Exit(0)
	}

	if *version {
		fmt.Printf("e-dnevnik-bot %v %v%v, built on: %v\n", GitTag, GitCommit, GitDirty, BuildTime)

		os.Exit(0)
	}

	if *tickInterval < DefaultTickInterval {
		logger.Info().Msgf("Poll interval is below %v, so I will default to %v", DefaultTickInterval, DefaultTickInterval)

		*tickInterval = time.Hour
	}
}
