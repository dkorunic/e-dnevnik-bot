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
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"

	"github.com/sirupsen/logrus"

	_ "github.com/KimMachineGun/automemlimit"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"go.uber.org/automaxprocs/maxprocs"
)

const (
	chanBufLen         = 500                   // broadcast channel buffer length
	exitDelay          = 5 * time.Second       // sleep time before giving up on cancellation
	logTimestampFormat = "02-01-2006 15:04:05" // logrus timestamp format
	testUsername       = "korisnik@test.domena"
	testSubject        = "Ovo je testni predmet"
	testDescription    = "Testni opis"
	testField          = "Testna vrijednost"
)

var (
	exitWithError atomic.Bool
	ErrMaxProc    = errors.New("failed to set GOMAXPROCS")
)

func main() {
	parseFlags()

	// TODO: Better systemd formatter integration
	formatter := &logrus.TextFormatter{
		TimestampFormat:  logTimestampFormat,
		FullTimestamp:    true,
		DisableTimestamp: false,
	}
	logrus.SetFormatter(formatter)
	logrus.SetOutput(os.Stdout)
	if *debug {
		logrus.SetLevel(logrus.DebugLevel)
	}

	// auto-configure GOMAXPROCS
	undo, err := maxprocs.Set()
	defer undo()
	if err != nil {
		logrus.Warnf("%v: %v", ErrMaxProc, err)
	}

	// context with signal integration
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// load TOML config
	config, err := loadConfig()
	if err != nil {
		logrus.Fatalf("Error loading configuration: %v", err)
	}

	// test mode: send messages and exit
	if *emulation {
		logrus.Info("Emulation/testing mode enabled, will try to send a test message")
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

		logrus.Info("Exiting with a success from the emulation.")

		return
	}

	// initial ticker delay of 1s
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	if *daemon {
		logrus.Infof("Service started, will collect information every %v", tickInterval)
	} else {
		logrus.Info("Doing a single run")
	}

	for {
		select {
		// in case of context cancellation, try to propagate and exit
		case <-ctx.Done():
			logrus.Info("Received stop signal, asking all routines to stop")
			ticker.Stop()
			go stop()
			if isatty.IsTerminal(os.Stdout.Fd()) || isatty.IsCygwinTerminal(os.Stdout.Fd()) {
				go spinner()
			}
			time.Sleep(exitDelay)
			if exitWithError.Load() {
				logrus.Warn("Exiting, during run some errors were encountered.")
				os.Exit(1) //nolint:gocritic
			}
			logrus.Info("Exiting with a success.")

			return
		case <-ticker.C:
			logrus.Info("Doing a scheduled run")
			ticker.Reset(tickInterval)

			// reset exit error status
			exitWithError.Store(false)

			// subjects/grades/exams scraper routines
			gradesScraped := make(chan msgtypes.Message, chanBufLen)
			var wgScrape sync.WaitGroup
			scrapers(ctx, &wgScrape, gradesScraped, config)

			// message/alert database checking routine
			gradesMsg := make(chan msgtypes.Message, chanBufLen)
			var wgFilter sync.WaitGroup
			msgDedup(ctx, &wgFilter, gradesScraped, gradesMsg)

			// messenger routines
			var wgMsg sync.WaitGroup
			msgSend(ctx, &wgMsg, gradesMsg, config)

			wgScrape.Wait()
			close(gradesScraped)

			wgFilter.Wait()
			wgMsg.Wait()

			if !*daemon {
				if exitWithError.Load() {
					logrus.Warn("Exiting, during run some errors were encountered.")
					os.Exit(1)
				}
				logrus.Info("Exiting with a success.")

				return
			}
			logrus.Info("Scheduled run completed, will sleep now")
		}
	}
}
