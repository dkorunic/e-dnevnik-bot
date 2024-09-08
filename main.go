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
	"context"
	"errors"
	"io/fs"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/messenger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dustin/go-humanize"
	sysdnotify "github.com/iguanesolutions/go-systemd/v5/notify"
	sysdwatchdog "github.com/iguanesolutions/go-systemd/v5/notify/watchdog"
	"github.com/mattn/go-isatty"
	"github.com/reiver/go-cast"
	"github.com/rs/zerolog"
	"go.uber.org/automaxprocs/maxprocs"
)

const (
	chanBufLen      = 500             // broadcast channel buffer length
	exitDelay       = 5 * time.Second // sleep time before giving up on cancellation
	testUsername    = "korisnik@test.domena"
	testSubject     = "Ovo je testni predmet"
	testDescription = "Testni opis"
	testField       = "Testna vrijednost"
	maxMemRatio     = 0.9
	scheduledActive = "Scheduled run in progress"
	scheduledSleep  = "Scheduled run completed, will sleep now"
)

var (
	exitWithError atomic.Bool
	ErrMaxProc    = errors.New("failed to set GOMAXPROCS")
	GitTag        = ""
	GitCommit     = ""
	GitDirty      = ""
	BuildTime     = ""
)

// init initializes the GitTag, GitCommit, GitDirty, and BuildTime variables.
//
// It trims leading and trailing white spaces from the values of GitTag, GitCommit,
// GitDirty, and BuildTime.
//
//nolint:gochecknoinits
func init() {
	GitTag = strings.TrimSpace(GitTag)
	GitCommit = strings.TrimSpace(GitCommit)
	GitDirty = strings.TrimSpace(GitDirty)
	BuildTime = strings.TrimSpace(BuildTime)
}

// fatalIfErrors is a Go function that checks if any errors were encountered during runtime.
//
// It checks the value of the exitWithError variable and if it is true, it logs a warning message
// and exits the program with an exit code of 1. If the exitWithError variable is false, it logs
// an info message and exits the program with an exit code of 0 (success).
func fatalIfErrors() {
	if exitWithError.Load() {
		logger.Fatal().Msg("Exiting, during run some errors were encountered.")
	}

	logger.Info().Msg("Exiting with a success.")
}

// main is the entry point of the application.
//
// It parses flags, sets the global log level, enables slow colored console logging,
// configures GOMAXPROCS, sets up a context with signal integration, loads the TOML config,
// checks Google Calendar setup, enables CPU profiling dump on exit, enables memory profile dump on exit,
// enters test mode if enabled, starts the service if in daemon mode, and runs scheduled tasks.
func main() {
	parseFlags()

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

	logger.Info().Msgf("e-dnevnik-bot %v %v%v, built on %v, with %v", GitTag, GitCommit, GitDirty,
		BuildTime, runtime.Version())

	// configure GOMEMLIMIT to 90% of available memory (Cgroups v2/v1 or system)
	limit, err := memlimit.SetGoMemLimitWithOpts(
		memlimit.WithRatio(maxMemRatio),
		memlimit.WithProvider(
			memlimit.ApplyFallback(
				memlimit.FromCgroup,
				memlimit.FromSystem,
			),
		),
	)

	if err != nil {
		logger.Warn().Msgf("Unable to get/set GOMEMLIMIT: %v", err)
	} else {
		logger.Debug().Msgf("GOMEMLIMIT is set to: %v", humanize.Bytes(uint64(limit)))
	}

	// configure GOMAXPROCS
	undo, err := maxprocs.Set()
	defer undo()

	if err != nil {
		logger.Warn().Msgf("%v: %v", ErrMaxProc, err)
	}

	logger.Debug().Msgf("GOMAXPROCS limit is set to: %v", runtime.GOMAXPROCS(0))

	if sysdnotify.IsEnabled() {
		logger.Debug().Msg("Detected and enabled systemd notify support")
	}

	// context with signal integration
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// load TOML config
	config, err := loadConfig()
	if err != nil {
		logger.Fatal().Msgf("Error loading configuration: %v", err)
	}

	// enable CPU profiling dump on exit
	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			logger.Fatal().Msgf("Error creating CPU profile: %v", err)
		}
		defer f.Close()

		if err := pprof.StartCPUProfile(f); err != nil {
			logger.Fatal().Msgf("Error starting CPU profile: %v", err)
		}
		defer pprof.StopCPUProfile()
	}

	// enable memory profile dump on exit
	if *memProfile != "" {
		f, err := os.Create(*memProfile)
		if err != nil {
			logger.Fatal().Msgf("Error trying to create memory profile: %v", err)
		}
		defer f.Close()

		defer func() {
			runtime.GC()

			if err := pprof.WriteHeapProfile(f); err != nil {
				logger.Fatal().Msgf("Error writing memory profile: %v", err)
			}
		}()
	}

	// Google Calendar API initial setup
	if config.calendarEnabled {
		checkCalendar(ctx, &config)
	}

	// test mode: send messages and exit
	if *emulation {
		logger.Info().Msg("Emulation/testing mode enabled, will try to send a test message")
		signal.Reset()

		gradesMsg := make(chan msgtypes.Message, chanBufLen)
		gradesMsg <- msgtypes.Message{
			Username: testUsername,
			Subject:  testSubject,
			Descriptions: []string{
				testDescription,
			},
			Fields: []string{
				testField,
			},
		}
		close(gradesMsg)

		var wgMsg sync.WaitGroup

		msgSend(ctx, &wgMsg, gradesMsg, config)
		wgMsg.Wait()

		logger.Info().Msg("Exiting with a success from the emulation.")

		return
	}

	// initial ticker delay of 1s
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	if *daemon {
		logger.Info().Msgf("Service started, will collect information every %v", tickInterval)
	} else {
		logger.Info().Msg("Service is not enabled, doing just a single run")
	}

	_ = sysdnotify.Ready()

	// systemd watchdog
	watchdog, _ := sysdwatchdog.New()
	if watchdog != nil {
		logger.Debug().Msg("Detected and enabled systemd watchdog support")

		go func() {
			ticker := watchdog.NewTicker()
			defer ticker.Stop()

			for {
				select {
				case <-ticker.C:
					_ = watchdog.SendHeartbeat()
				case <-ctx.Done():
					return
				}
			}
		}()
	}

	for {
		select {
		// in case of context cancellation, try to propagate and exit
		case <-ctx.Done():
			logger.Info().Msg("Received stop signal, asking all routines to stop")
			ticker.Stop()

			_ = sysdnotify.Stopping()

			go stop()

			if isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()) {
				go spinner()
			}

			time.Sleep(exitDelay)
			fatalIfErrors()

			return
		case <-ticker.C:
			logger.Info().Msg(scheduledActive)
			ticker.Reset(*tickInterval)

			_ = sysdnotify.Status(scheduledActive)

			// reset exit error status
			exitWithError.Store(false)

			gradesScraped := make(chan msgtypes.Message, chanBufLen)
			gradesMsg := make(chan msgtypes.Message, chanBufLen)

			var wgVersion, wgScrape, wgFilter, wgMsg sync.WaitGroup

			// self-check
			versionCheck(ctx, &wgVersion)

			// subjects/grades/exams scraper routines
			scrapers(ctx, &wgScrape, gradesScraped, config)

			// message/alert database checking routine
			msgDedup(ctx, &wgFilter, gradesScraped, gradesMsg)

			// messenger routines
			msgSend(ctx, &wgMsg, gradesMsg, config)

			wgScrape.Wait()
			close(gradesScraped)

			wgFilter.Wait()
			wgMsg.Wait()
			wgVersion.Wait()

			if !*daemon {
				fatalIfErrors()

				return
			}

			logger.Info().Msg(scheduledSleep)

			_ = sysdnotify.Status(scheduledSleep)
		}
	}
}

// checkCalendar checks the calendar configuration and enables or disables the calendar integration based on the existence of the Google Calendar API credentials file and token file.
//
// Parameters:
// - config: a pointer to the tomlConfig struct containing the configuration settings.
// - ctx: the context object for cancellation and timeout.
func checkCalendar(ctx context.Context, config *tomlConfig) {
	if config == nil {
		return
	}

	if _, err := os.Stat(*calTokFile); errors.Is(err, fs.ErrNotExist) {
		// check if we are running under a terminal
		fd := os.Stdout.Fd()
		if os.Getenv("TERM") == "dumb" || (!isatty.IsTerminal(fd) && !isatty.IsCygwinTerminal(fd)) {
			logger.Error().Msgf("Google Calendar API token file not found and first run requires running under a terminal. Disabling calendar integration.")

			config.calendarEnabled = false
		} else {
			// early Google Calendar API initialization and token refresh
			_, _, err := messenger.InitCalendar(ctx, *calTokFile, config.Calendar.Name)
			if err != nil {
				logger.Error().Msgf("Error initializing Google Calendar API: %v. Disabling calendar integration.", err)

				config.calendarEnabled = false
			}
		}
	}
}
