// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

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

	// bgWG tracks background goroutines so shutdown bounds their wait via exitDelay.
	bgWG sync.WaitGroup
)

// fatalIfErrors exits non-zero (via Fatal) if any cycle set exitWithError,
// otherwise logs success. Terminal call — does not return on the error path.
func fatalIfErrors() {
	if exitWithError.Load() {
		logger.Fatal().Msg("Exiting, during run some errors were encountered.")
	}

	logger.Info().Msg("Exiting with a success.")
}

// main wires up config, logging, memory limits, signal handling, and profiling,
// runs first-run Calendar/WhatsApp setup, then either does a single run or
// drives the daemon poll loop until signalled.
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
		fatalIfErrors()

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
			// Surface "overdue" on overrun so operators see a live signal, not a frozen status.
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

			// exitWithError latches for the process lifetime (not reset per
			// cycle) so a daemon that errored in any cycle exits non-zero.

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

			// On overrun, emit "overdue" instead of a useless "Next run in 0 second".
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

// startSystemdWatchdog, when a watchdog is configured, spawns a bgWG-tracked
// goroutine that sends heartbeats until ctx is cancelled.
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

// testSingleRun pushes one synthetic message through the full send pipeline so
// operators can verify messenger credentials and formatting without scraping.
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

// durationRandJitter scales x by a continuous factor in [0.9, 1.1) so
// concurrent daemons spread their polls instead of hitting the portal in
// lockstep. Continuous (not stepped) to avoid aliasing on a few wake times.
func durationRandJitter(x time.Duration) time.Duration {
	//nolint:gosec,mnd
	return time.Duration(float64(x) * (0.9 + 0.2*rand.Float64()))
}
