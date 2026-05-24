// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/hako/durafmt"
	"github.com/peterbourgon/ff/v4"
	"github.com/peterbourgon/ff/v4/ffhelp"
)

const (
	DefaultConfFile      = ".e-dnevnik.toml"     // default configuration filename
	DefaultCalendarToken = "calendar_token.json" // default Google Calendar token file
	DefaultTickInterval  = 1 * time.Hour         // default (and minimal permitted value) is 1 tick per 1h
	DefaultRetries       = 3                     // default retry attempts
	DefaultDBPath        = ".e-dnevnik.db"       // default SQLite DB
)

var (
	debug, debugEvents, daemon, help, emulation, colorLogs, version, jitter, readingList *bool
	confFile, dbFile, cpuProfile, memProfile, calTokFile                                 *string
	tickInterval, relevancePeriod                                                        *time.Duration
	retries                                                                              *uint
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
	readingList = fs.BoolLong("readinglist", "send reading list alerts")
	jitter = fs.BoolDefault('j', "jitter", true, "enable slight (up to 10%) jitter for tick intervals")

	confFile = fs.String('f', "conffile", DefaultConfFile, "configuration file (in TOML)")
	dbFile = fs.String('b', "database", DefaultDBPath, "alert database file")
	calTokFile = fs.String('g', "calendartoken", DefaultCalendarToken, "Google Calendar token file")
	cpuProfile = fs.String('c', "cpuprofile", "", "CPU profile output file")
	memProfile = fs.String('m', "memprofile", "", "memory profile output file")

	tickInterval = fs.Duration('i', "interval", DefaultTickInterval, "interval between polls when in daemon mode")
	relevancePeriod = fs.Duration('p', "relevance", 0, "maximum relevance period for grade and exam events (0 = unlimited)")

	retries = fs.Uint('r', "retries", DefaultRetries, "number of retry attempts on error")

	var err error

	if err = ff.Parse(fs, os.Args[1:]); err != nil {
		// --help requested via ff: exit 0, not 1.
		if errors.Is(err, ff.ErrHelp) {
			fmt.Printf("%s\n", ffhelp.Flags(fs))

			os.Exit(0)
		}

		fmt.Printf("%s\n", ffhelp.Flags(fs))
		fmt.Printf("Error: %v\n", err)

		os.Exit(1)
	}

	if *help {
		fmt.Printf("%s\n", ffhelp.Flags(fs))

		os.Exit(0)
	}

	if *version {
		fmt.Printf("e-dnevnik-bot %v %v%v, built on %v, with %v\n", GitTag, GitCommit, GitDirty,
			BuildTime, runtime.Version())

		os.Exit(0)
	}

	if *tickInterval < DefaultTickInterval {
		logger.Info().Msgf("Poll interval is below %v, so I will default to %v",
			durafmt.Parse(DefaultTickInterval).String(), durafmt.Parse(DefaultTickInterval).String())

		*tickInterval = DefaultTickInterval
	}

	if *relevancePeriod < 0 {
		logger.Info().Msg("Negative relevance period is not valid, resetting to unlimited (0)")

		*relevancePeriod = 0
	}

	// retry-go treats Attempts(0) as unlimited; clamp to bound retries.
	if *retries == 0 {
		logger.Info().Msg("Retries flag set to 0; clamping to 1 (no retries) — retry-go interprets 0 as unlimited")

		*retries = 1
	}

	if *debugEvents {
		*debug = true
	}
}
