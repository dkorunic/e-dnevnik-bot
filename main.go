// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"slices"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/KimMachineGun/automemlimit/memlimit"
	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/messenger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dustin/go-humanize"
	"github.com/hako/durafmt"
	sysdnotify "github.com/iguanesolutions/go-systemd/v6/notify"
	sysdwatchdog "github.com/iguanesolutions/go-systemd/v6/notify/watchdog"
)

const (
	chanBufLen       = 500              // scrape→dedup and dedup→fan-out channel buffer
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

	// Background goroutines, so shutdown can bound their wait by exitDelay.
	bgWG sync.WaitGroup
)

// fatalIfErrors exits non-zero if any cycle latched a failure. Terminal on that
// path.
//
// flush runs first because logger.Fatal is os.Exit, which skips defers: leaving
// pprof teardown to one would lose the profile of every failed run.
func fatalIfErrors(flush func()) {
	flush()

	if exitWithError.Load() {
		logger.Fatal().Msg("Exiting, during run some errors were encountered.")
	}

	logger.Info().Msg("Exiting with a success.")
}

// startProfiling honours -c/-m and returns the flush that finalises them. Not a
// defer (see fatalIfErrors), once-guarded because both callers invoke it, and
// reverse-ordered to match the defers it replaces.
func startProfiling() func() {
	var flushes []func()

	if *cpuProfile != "" {
		f, err := os.Create(*cpuProfile)
		if err != nil {
			logger.Fatal().Msgf("Error creating CPU profile: %v", err)
		}

		if err := pprof.StartCPUProfile(f); err != nil {
			_ = f.Close()

			logger.Fatal().Msgf("Error starting CPU profile: %v", err)
		}

		flushes = append(flushes, func() {
			pprof.StopCPUProfile()

			if err := f.Close(); err != nil {
				logger.Error().Msgf("Error closing CPU profile: %v", err)
			}
		})
	}

	if *memProfile != "" {
		f, err := os.Create(*memProfile)
		if err != nil {
			logger.Fatal().Msgf("Error trying to create memory profile: %v", err)
		}

		flushes = append(flushes, func() {
			runtime.GC()

			// Not Fatal: it would pre-empt fatalIfErrors' exit.
			if err := pprof.WriteHeapProfile(f); err != nil {
				logger.Error().Msgf("Error writing memory profile: %v", err)
			}

			if err := f.Close(); err != nil {
				logger.Error().Msgf("Error closing memory profile: %v", err)
			}
		})
	}

	var once sync.Once

	return func() {
		once.Do(func() {
			for _, flush := range slices.Backward(flushes) {
				flush()
			}
		})
	}
}

// main wires up config, logging, memory limits, signals and profiling, runs
// first-run setup, then does a single run or drives the poll loop.
func main() {
	parseFlags()

	initLog()

	// After initLog: these answer the command line, so they must honour the
	// level and format it asked for.
	clampFlags()

	logger.Info().Msgf("e-dnevnik-bot %v %v%v, built on %v, with %v", GitTag, GitCommit, GitDirty,
		BuildTime, runtime.Version())

	// 90% of cgroup or system memory, to play nicely in containers.
	limit, err := memlimit.Set(
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

	// For the messengers that rewrite the file in place.
	ctx = context.WithValue(ctx, messenger.ConfFileKey, *confFile)

	flushProfiles := startProfiling()
	defer flushProfiles()

	// Interactive: must run on the main goroutine.
	if cfg.CalendarEnabled {
		checkCalendar(ctx, &cfg)
	}

	// Interactive: must run on the main goroutine.
	if cfg.WhatsAppEnabled {
		checkWhatsApp(ctx, &cfg)
	}

	if *emulation {
		testSingleRun(ctx, cfg)
		fatalIfErrors(flushProfiles)

		return
	}

	// First poll fires almost at once; Reset installs the real interval.
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	// Countdown to the next scrape: paused during runs, stopped until
	// nextRunAt is set.
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
			awaitShutdown(stop, ticker, statusTicker)
			fatalIfErrors(flushProfiles)

			return
		case <-statusTicker.C:
			// On overrun, say so rather than freeze the status.
			if remaining := time.Until(nextRunAt); remaining > 0 {
				_ = sysdnotify.Status(fmt.Sprintf(scheduledNext,
					durafmt.Parse(remaining.Round(time.Second)).String()))
			} else {
				_ = sysdnotify.Status(scheduledOverdue)
			}
		case <-ticker.C:
			// Paused while scraping.
			statusTicker.Stop()

			logger.Info().Msg(scheduledActive)

			// Spreads concurrent daemons off the portal's lockstep.
			nextInterval := *tickInterval
			if *jitter {
				nextInterval = durationRandJitter(*tickInterval)
			}

			ticker.Reset(nextInterval)
			nextRunAt = time.Now().Add(nextInterval)

			_ = sysdnotify.Status(scheduledActive)

			runPollCycle(ctx, cfg)

			if !*daemon {
				fatalIfErrors(flushProfiles)

				return
			}

			announceIdleWindow(ctx, statusTicker, nextRunAt)
		}
	}
}

// announceIdleWindow reports the wait until the next poll, or an overrun, and
// resumes the countdown.
//
// Silent once ctx is cancelled: a cycle cut short by SIGTERM has no idle window,
// and announcing one tells the operator it is sleeping while it is exiting.
func announceIdleWindow(ctx context.Context, statusTicker *time.Ticker, nextRunAt time.Time) {
	if ctx.Err() != nil {
		return
	}

	var scheduledSleep string
	if remaining := time.Until(nextRunAt); remaining > 0 {
		scheduledSleep = fmt.Sprintf(scheduledNext, durafmt.Parse(remaining.Round(time.Second)).String())
	} else {
		scheduledSleep = scheduledOverdue
	}

	logger.Info().Msg(scheduledSleep)
	_ = sysdnotify.Status(scheduledSleep)

	// Stop()+Reset() leave a buffered tick behind.
	select {
	case <-statusTicker.C:
	default:
	}

	statusTicker.Reset(statusInterval)
}

// awaitShutdown drains bgWG under an exitDelay ceiling, so a wedged goroutine
// cannot stall exit. Only bgWG: the per-cycle waitgroups drained in
// runPollCycle.
func awaitShutdown(stop context.CancelFunc, ticker, statusTicker *time.Ticker) {
	logger.Info().Msg("Received stop signal, asking all routines to stop")
	ticker.Stop()
	statusTicker.Stop()

	_ = sysdnotify.Stopping()

	stop()

	// The wait can run seconds, and silence reads as a hang.
	var spinnerDone chan struct{}

	if isTerminal() {
		spinnerDone = make(chan struct{})
		go spinner(spinnerDone)
	}

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
}

// runPollCycle runs one scrape→dedup→send pipeline on a freshly opened DB.
//
// Teardown order is load-bearing: gradesScraped closes only once the scrapers
// have finished, that close being what unblocks msgDedup's range, and the DB
// must outlive every stage since msgDedup and the messengers both write to it.
// exitWithError latches for the process lifetime, so a daemon that errored in
// any cycle still exits non-zero.
func runPollCycle(ctx context.Context, cfg config.TomlConfig) {
	gradesScraped := make(chan msgtypes.Message, chanBufLen)
	gradesMsg := make(chan msgtypes.Message, chanBufLen)

	var wgVersion, wgScrape, wgFilter, wgMsg sync.WaitGroup

	versionCheck(ctx, &wgVersion)

	eDB, err := openDB(ctx, *dbFile)
	if err != nil {
		// Without the dedup store nothing can tell a new event from a seen one,
		// so skip the cycle rather than run blind. versionCheck is already in
		// flight and still has to be awaited.
		logger.Error().Msgf("Unable to open application database, skipping this cycle: %v", err)
		exitWithError.Store(true)

		wgVersion.Wait()

		return
	}

	scrapeStage(ctx, &wgScrape, gradesScraped, cfg)

	msgDedup(ctx, eDB, &wgFilter, gradesScraped, gradesMsg)

	msgSend(ctx, eDB, &wgMsg, gradesMsg, cfg)

	wgScrape.Wait()
	close(gradesScraped)

	wgFilter.Wait()
	wgMsg.Wait()
	wgVersion.Wait()

	closeDB(eDB)
}

// startSystemdWatchdog heartbeats until ctx is cancelled, when one is
// configured.
func startSystemdWatchdog(ctx context.Context) {
	watchdog, _ := sysdwatchdog.New()
	if watchdog != nil {
		logger.Debug().Msg("Detected and enabled systemd watchdog support")

		// bgWG, so shutdown awaits it under a bounded timeout.
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

// testSingleRun pushes one synthetic message through the send pipeline, so
// credentials and formatting can be checked without scraping. The signal ctx
// stays live, so SIGTERM drains through the messengers' queue persistence
// rather than killing the process mid-send.
func testSingleRun(ctx context.Context, config config.TomlConfig) {
	logger.Info().Msg("Emulation/testing mode enabled, will try to send a test message")

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

	eDB, err := openDB(ctx, *dbFile)
	if err != nil {
		logger.Error().Msgf("Unable to open application database: %v", err)
		exitWithError.Store(true)

		return
	}

	msgSend(ctx, eDB, &wgMsg, gradesMsg, config)

	wgMsg.Wait()

	closeDB(eDB)

	logger.Info().Msg("Exiting with a success from the emulation.")
}

// durationRandJitter scales x by a factor in [0.9, 1.1), spreading concurrent
// daemons off the portal's lockstep. Continuous, not stepped: a stepped variant
// aliases onto a handful of wake times.
func durationRandJitter(x time.Duration) time.Duration {
	//nolint:gosec,mnd
	return time.Duration(float64(x) * (0.9 + 0.2*rand.Float64()))
}
