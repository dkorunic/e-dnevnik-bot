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
	"fmt"
	"math/rand/v2" //nolint:gosec
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/dkorunic/e-dnevnik-bot/config"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/messenger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dustin/go-humanize"
	"github.com/hako/durafmt"
	sysdnotify "github.com/iguanesolutions/go-systemd/v6/notify"
	sysdwatchdog "github.com/iguanesolutions/go-systemd/v6/notify/watchdog"
)

const (
	chanBufLen       = 500              // broadcast channel buffer length
	exitDelay        = 10 * time.Second // sleep time before giving up on cancellation
	statusInterval   = 1 * time.Minute  // cadence of sysd status countdown updates between runs
	testUsername     = "korisnik@test.domena"
	testSubject      = "Ovo je testni predmet"
	testDescription  = "Testni opis"
	testField        = "Testna vrijednost"
	maxMemRatio      = 0.9
	scheduledActive  = "Scheduled run in progress"
	scheduledNext    = "Next scheduled run in %s"
	scheduledOverdue = "Scheduled run is overdue"
)

var (
	exitWithError atomic.Bool
	GitTag        = ""
	GitCommit     = ""
	GitDirty      = ""
	BuildTime     = ""

	// bgWG tracks long-running background goroutines (e.g. the systemd
	// watchdog) so that the shutdown path can give them a bounded chance to
	// exit cleanly instead of sleeping unconditionally for exitDelay.
	bgWG sync.WaitGroup
)

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

	initLog()

	logger.Info().Msgf("e-dnevnik-bot %v %v%v, built on %v, with %v", GitTag, GitCommit, GitDirty,
		BuildTime, runtime.Version())

	// Cap heap at 90% of cgroup/system memory to play nice with containers.
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
		logger.Debug().Msgf("GOMEMLIMIT is set to: %v", humanize.Bytes(uint64(limit))) //nolint:gosec
	}

	logger.Debug().Msgf("GOMAXPROCS limit is set to: %v", runtime.GOMAXPROCS(0))

	if sysdnotify.IsEnabled() {
		logger.Debug().Msg("Detected and enabled systemd notify support")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := config.LoadConfig(*confFile)
	if err != nil {
		logger.Fatal().Msgf("Error loading configuration: %v", err)
	}

	// Pass config path to messengers that reload credentials on token refresh.
	ctx = context.WithValue(ctx, messenger.ConfFileKey, *confFile)

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

	// Interactive OAuth flow must run on the main goroutine.
	if cfg.CalendarEnabled {
		checkCalendar(ctx, &cfg)
	}

	// Pairing/QR flow must run on the main goroutine.
	if cfg.WhatsAppEnabled {
		checkWhatsApp(ctx, &cfg)
	}

	if *emulation {
		testSingleRun(ctx, cfg)

		return
	}

	// Fire the first poll almost immediately; real interval takes over after Reset.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Countdown to next scrape; paused during runs, starts stopped until nextRunAt is set.
	statusTicker := time.NewTicker(statusInterval)
	statusTicker.Stop()
	defer statusTicker.Stop()

	var nextRunAt time.Time

	if *daemon {
		interval := durafmt.Parse(*tickInterval).String()
		if *jitter {
			logger.Info().Msgf("Service started, will collect information every %v (with random jitter up to +-10%%)",
				interval)
		} else {
			logger.Info().Msgf("Service started, will collect information every %v",
				interval)
		}
	} else {
		logger.Info().Msg("Service is not enabled, doing just a single run")
	}

	_ = sysdnotify.Ready()

	startSystemdWatchdog(ctx)

	for {
		select {
		case <-ctx.Done():
			logger.Info().Msg("Received stop signal, asking all routines to stop")
			ticker.Stop()
			statusTicker.Stop()

			_ = sysdnotify.Stopping()

			stop()

			var spinnerDone chan struct{}
			if isTerminal() {
				spinnerDone = make(chan struct{})
				go spinner(spinnerDone)
			}

			// Bounded wait — stuck goroutine must not stall shutdown.
			bgDone := make(chan struct{})
			go func() {
				bgWG.Wait()
				close(bgDone)
			}()

			select {
			case <-bgDone:
			case <-time.After(exitDelay):
			}

			if spinnerDone != nil {
				close(spinnerDone)
			}

			fatalIfErrors()

			return
		case <-statusTicker.C:
			// Countdown heartbeat while idling between scheduled runs.
			// If the scrape overran nextRunAt, surface "overdue" so operators
			// see a live signal instead of a frozen prior status string.
			if remaining := time.Until(nextRunAt); remaining > 0 {
				_ = sysdnotify.Status(fmt.Sprintf(scheduledNext,
					durafmt.Parse(remaining.Round(time.Second)).String()))
			} else {
				_ = sysdnotify.Status(scheduledOverdue)
			}
		case <-ticker.C:
			// Pause countdown while scraping.
			statusTicker.Stop()

			logger.Info().Msg(scheduledActive)

			// Jitter spreads concurrent daemons so they don't hammer the portal together.
			nextInterval := *tickInterval
			if *jitter {
				nextInterval = durationRandJitter(*tickInterval)
			}

			ticker.Reset(nextInterval)
			nextRunAt = time.Now().Add(nextInterval)

			_ = sysdnotify.Status(scheduledActive)

			exitWithError.Store(false)

			gradesScraped := make(chan msgtypes.Message, chanBufLen)
			gradesMsg := make(chan msgtypes.Message, chanBufLen)

			var wgVersion, wgScrape, wgFilter, wgMsg sync.WaitGroup

			versionCheck(ctx, &wgVersion)

			eDB := openDB(ctx, *dbFile)

			scrapers(ctx, &wgScrape, gradesScraped, cfg)

			msgDedup(ctx, eDB, &wgFilter, gradesScraped, gradesMsg)

			msgSend(ctx, eDB, &wgMsg, gradesMsg, cfg)

			wgScrape.Wait()
			close(gradesScraped)

			wgFilter.Wait()
			wgMsg.Wait()
			wgVersion.Wait()

			closeDB(eDB)

			if !*daemon {
				fatalIfErrors()

				return
			}

			// On overrun (scrape ran past its scheduled window) emit the
			// "overdue" string instead of "Next scheduled run in 0 second",
			// which is technically true but useless to operators.
			var scheduledSleep string
			if remaining := time.Until(nextRunAt); remaining > 0 {
				scheduledSleep = fmt.Sprintf(scheduledNext, durafmt.Parse(remaining.Round(time.Second)).String())
			} else {
				scheduledSleep = scheduledOverdue
			}

			logger.Info().Msg(scheduledSleep)
			_ = sysdnotify.Status(scheduledSleep)

			// Drain stale tick; Stop()+Reset() don't flush the buffered value.
			select {
			case <-statusTicker.C:
			default:
			}

			// Resume countdown for the idle window.
			statusTicker.Reset(statusInterval)
		}
	}
}

// startSystemdWatchdog sets up the systemd watchdog for the application.
//
// It initializes a systemd watchdog and starts a goroutine that sends
// periodic heartbeat signals to systemd. The function listens for context
// cancellation, upon which it stops sending heartbeats and exits the
// goroutine. This function is useful for ensuring the application is
// alive and responsive, as monitored by systemd.
//
// Parameters:
// - ctx: the context object for cancellation and timeout.
func startSystemdWatchdog(ctx context.Context) {
	watchdog, _ := sysdwatchdog.New()
	if watchdog != nil {
		logger.Debug().Msg("Detected and enabled systemd watchdog support")

		// Tracked in bgWG so shutdown awaits it with a bounded timeout.
		bgWG.Go(func() {
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
		})
	}
}

// testSingleRun performs a single run in emulation mode. It sends a single test
// message to each messenger and exits after that. It is meant to be used for
// testing and debugging purposes only.
//
// The function takes a context.Context and a TomlConfig as parameters. The
// context is used to cancel the function early if the User cancels it.
//
// The function will log a message when it is called and another one when it is
// exiting.
func testSingleRun(ctx context.Context, config config.TomlConfig) {
	logger.Info().Msg("Emulation/testing mode enabled, will try to send a test message")
	signal.Reset(os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	gradesMsg := make(chan msgtypes.Message, chanBufLen)
	gradesMsg <- msgtypes.Message{
		Code:     msgtypes.Grade,
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

	eDB := openDB(ctx, *dbFile)

	msgSend(ctx, eDB, &wgMsg, gradesMsg, config)

	wgMsg.Wait()

	closeDB(eDB)

	logger.Info().Msg("Exiting with a success from the emulation.")
}

// durationRandJitter adds a random jitter to x in the range [0.9 * x, 1.1 * x].
//
// This is useful for spreading out events in time, e.g. when multiple instances
// of this program are running at the same time and you don't want them to hit
// the same external service at the same time.
//
// The jitter factor is drawn from a continuous uniform distribution over
// [0.9, 1.1) via rand.Float64(), not a 21-step integer distribution, so
// multiple concurrent daemons do not cluster on a small number of discrete
// wake times. The randomness is generated using the math/rand/v2 package.
func durationRandJitter(x time.Duration) time.Duration {
	//nolint:gosec,mnd
	return time.Duration(float64(x) * (0.9 + 0.2*rand.Float64()))
}
