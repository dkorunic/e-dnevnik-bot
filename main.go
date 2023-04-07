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
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	_ "github.com/KimMachineGun/automemlimit"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/mattn/go-isatty"
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
	envGOMEMLIMIT   = "GOMEMLIMIT"
)

var (
	exitWithError atomic.Bool
	ErrMaxProc    = errors.New("failed to set GOMAXPROCS")
	GitTag        = ""
	GitCommit     = ""
	GitDirty      = ""
	BuildTime     = ""
)

func fatalIfErrors() {
	if exitWithError.Load() {
		logger.Warn().Msg("Exiting, during run some errors were encountered.")
		os.Exit(1)
	}

	logger.Info().Msg("Exiting with a success.")
}

func main() {
	parseFlags()

	// set global log level
	logLevel := zerolog.InfoLevel
	if *debug {
		logLevel = zerolog.DebugLevel
	} else {
		if v, ok := os.LookupEnv("LOG_LEVEL"); ok {
			if l, err := strconv.Atoi(v); err != nil {
				logLevel = zerolog.Level(l)
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

	logger.Info().Msgf("e-dnevnik-bot %v %v%v, built on: %v", GitTag, GitCommit, GitDirty, BuildTime)

	// auto-configure GOMAXPROCS
	undo, err := maxprocs.Set()
	defer undo()

	if err != nil {
		logger.Warn().Msgf("%v: %v", ErrMaxProc, err)
	}

	logger.Debug().Msgf("GOMAXPROCS limit is set to: %v", runtime.GOMAXPROCS(0))

	if v, ok := os.LookupEnv(envGOMEMLIMIT); ok {
		logger.Debug().Msgf("GOMEMLIMIT is set to: %v", v)
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

	for {
		select {
		// in case of context cancellation, try to propagate and exit
		case <-ctx.Done():
			logger.Info().Msg("Received stop signal, asking all routines to stop")
			ticker.Stop()

			go stop()

			if isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()) {
				go spinner()
			}

			time.Sleep(exitDelay)
			fatalIfErrors()

			return
		case <-ticker.C:
			logger.Info().Msg("Scheduled run in progress")
			ticker.Reset(tickInterval)

			// reset exit error status
			exitWithError.Store(false)

			gradesScraped := make(chan msgtypes.Message, chanBufLen)
			gradesMsg := make(chan msgtypes.Message, chanBufLen)

			var wgScrape, wgFilter, wgMsg sync.WaitGroup

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

			if !*daemon {
				fatalIfErrors()

				return
			}

			logger.Info().Msg("Scheduled run completed, will sleep now")
		}
	}
}
