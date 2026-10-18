// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/messenger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"google.golang.org/api/calendar/v3"
)

// setDBFile points the package-level database flag at a fresh temp path.
// Callers must not run in parallel — the pointer is process-global.
// setEmulationDB points emulation at a kept database in t.TempDir, so a test
// can inspect what the messengers queued.
// Not parallel: swaps the package-level emulationDB seam.
func setEmulationDB(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)

	orig := emulationDB
	emulationDB = func() (string, func(), error) { return path, func() {}, nil }

	t.Cleanup(func() { emulationDB = orig })

	return path
}

func setDBFile(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)

	orig := dbFile
	dbFile = &path

	t.Cleanup(func() { dbFile = orig })

	return path
}

// setRetries points the package-level retries flag at v. msgSend dereferences
// it when building any messenger config, so tests that enable a messenger must
// set it or hit a nil-pointer dereference.
// Callers must not run in parallel — the pointer is process-global.
func setRetries(t *testing.T, v uint) {
	t.Helper()

	orig := retries
	retries = &v

	t.Cleanup(func() { retries = orig })
}

// resetExitLatch clears the process-wide failure latch and restores it after
// the test, so one test's expected messenger error cannot leak into another.
func resetExitLatch(t *testing.T) {
	t.Helper()

	orig := exitWithError.Load()

	t.Cleanup(func() { exitWithError.Store(orig) })

	exitWithError.Store(false)
}

// setCalTokFile points the package-level token-path flag at path.
// Callers must not run in parallel — the pointer is process-global.
func setCalTokFile(t *testing.T, path string) {
	t.Helper()

	orig := calTokFile
	calTokFile = &path

	t.Cleanup(func() { calTokFile = orig })
}

// runWithinTimeout fails instead of hanging when fn deadlocks.
func runWithinTimeout(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		fn()
	}()

	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%v did not complete within %v", what, d)
	}
}

// TestRunPollCycleCompletes exercises one whole cycle. The teardown ordering is
// the load-bearing part: gradesScraped may only be closed once the scrapers have
// finished (that close is what ends msgDedup's range), and the database must
// outlive every stage because msgDedup and the messengers both write to it.
// Getting the order wrong deadlocks or panics on a closed database rather than
// failing visibly, so the assertion is simply that the cycle unwinds.
// Not parallel: mutates package-level flag pointers.
func TestRunPollCycleCompletes(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	resetExitLatch(t)
	setDBFile(t, "poll.db")

	runWithinTimeout(t, 60*time.Second, "runPollCycle", func() {
		runPollCycle(t.Context(), config.TomlConfig{})
	})

	if exitWithError.Load() {
		t.Error("an empty cycle latched exitWithError; a config with no users or messengers is not a failure")
	}
}

// TestRunPollCycleWithDeferredCalendar runs a cycle with a messenger sink
// attached, so the fan-out and its teardown are covered rather than the
// degenerate zero-sink path. With no configured users nothing is scraped, so
// the queue must stay empty — a non-empty queue would mean the pipeline
// invented an event.
// Not parallel: mutates package-level flag pointers.
func TestRunPollCycleWithDeferredCalendar(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	path := setDBFile(t, "poll-deferred.db")

	runWithinTimeout(t, 60*time.Second, "runPollCycle", func() {
		runPollCycle(t.Context(), config.TomlConfig{CalendarDeferred: true})
	})

	eDB, err := sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("reopening the cycle database failed — closeDB may not have released it: %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	if got := queue.FetchFailedMsgs(t.Context(), eDB, messenger.CalendarQueueName); len(got) != 0 {
		t.Errorf("queue holds %+v after a cycle with no configured users, want nothing", got)
	}
}

// TestRunPollCycleIsRepeatable: a daemon runs this on every tick, reopening the
// same database each time. A leaked handle or a stale first-run flag would
// surface on the second pass, not the first.
// Not parallel: mutates package-level flag pointers.
func TestRunPollCycleIsRepeatable(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	setDBFile(t, "poll-repeat.db")

	for i := range 3 {
		runWithinTimeout(t, 60*time.Second, "runPollCycle", func() {
			runPollCycle(t.Context(), config.TomlConfig{CalendarDeferred: true})
		})

		if exitWithError.Load() {
			t.Fatalf("cycle %d latched exitWithError with no configured users or messengers", i)
		}
	}
}

// TestTestSingleRunDeliversSyntheticMessage covers the -t/--test emulation
// path: one synthetic message goes through the real send pipeline so operators
// can verify credentials without waiting for a scrape.
// Not parallel: mutates package-level flag pointers.
func TestTestSingleRunDeliversSyntheticMessage(t *testing.T) {
	resetExitLatch(t)
	setRetries(t, 1)

	path := setEmulationDB(t, "emulation.db")

	// Slack with no token drains its channel into its queue and returns, which
	// makes the synthetic message observable without touching the network.
	runWithinTimeout(t, 60*time.Second, "testSingleRun", func() {
		testSingleRun(t.Context(), config.TomlConfig{SlackEnabled: true})
	})

	eDB, err := sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("reopening the emulation database failed: %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	got := queue.FetchFailedMsgs(t.Context(), eDB, messenger.SlackQueueName)
	if len(got) != 1 {
		t.Fatalf("messenger saw %d messages, want the 1 synthetic message", len(got))
	}

	if got[0].Msg.Username != testUsername || got[0].Msg.Subject != testSubject {
		t.Errorf("synthetic message = %+v, want username %q and subject %q",
			got[0].Msg, testUsername, testSubject)
	}
}

// TestTestSingleRunBypassesDedup is the point of emulation mode: the synthetic
// message must reach the messengers on every invocation. Routing it through the
// dedup store would make the second run silent and the mode useless for
// verifying credentials.
// Not parallel: mutates package-level flag pointers.
func TestTestSingleRunBypassesDedup(t *testing.T) {
	resetExitLatch(t)
	setRetries(t, 1)

	path := setEmulationDB(t, "emulation-twice.db")

	const runs = 2

	for range runs {
		runWithinTimeout(t, 60*time.Second, "testSingleRun", func() {
			testSingleRun(t.Context(), config.TomlConfig{SlackEnabled: true})
		})
	}

	eDB, err := sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("reopening the emulation database failed: %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	if got := queue.FetchFailedMsgs(t.Context(), eDB, messenger.SlackQueueName); len(got) != runs {
		t.Errorf("messenger saw %d messages over %d emulation runs, want %d — emulation must not be filtered by the dedup store, or it stops verifying credentials after the first use",
			len(got), runs, runs)
	}
}

// TestTestSingleRunLeavesRealDatabaseAlone: a test send that fails is queued,
// and queued in the real database the next normal run would deliver the fake
// grade to every recipient. Emulation must not touch that database at all.
// Not parallel: mutates package-level flag pointers.
func TestTestSingleRunLeavesRealDatabaseAlone(t *testing.T) {
	resetExitLatch(t)
	setRetries(t, 1)

	path := setDBFile(t, "real.db")

	// Slack with no token queues the synthetic message: the failure case.
	runWithinTimeout(t, 60*time.Second, "testSingleRun", func() {
		testSingleRun(t.Context(), config.TomlConfig{SlackEnabled: true})
	})

	if matches, _ := filepath.Glob(path + "*"); len(matches) != 0 {
		t.Errorf("emulation created %v next to the real database; a queued test message there reaches real recipients next run", matches)
	}
}

// TestScrapersWithNoUsers: an empty user list must spawn nothing and leave the
// run unflagged, so a config with messengers but no users is not a hard error.
// Not parallel: reads the package-level exitWithError latch.
func TestScrapersWithNoUsers(t *testing.T) {
	orig := exitWithError.Load()

	t.Cleanup(func() { exitWithError.Store(orig) })

	exitWithError.Store(false)

	ch := make(chan msgtypes.Message, 1)

	var wg sync.WaitGroup

	scrapers(t.Context(), &wg, ch, config.TomlConfig{}, nil)

	runWithinTimeout(t, 30*time.Second, "scrapers", wg.Wait)

	if exitWithError.Load() {
		t.Error("scrapers latched exitWithError for an empty user list")
	}

	if len(ch) != 0 {
		t.Errorf("scrapers emitted %d messages for an empty user list, want 0", len(ch))
	}
}

// TestIsTerminal: the go test harness runs with a non-TTY stdout, so this is
// the branch that gates first-run OAuth in headless daemons. TERM=dumb must
// also read as non-interactive.
// Not parallel: mutates the TERM environment variable.
func TestIsTerminal(t *testing.T) {
	t.Setenv("TERM", "dumb")

	if isTerminal() {
		t.Error("isTerminal() = true with TERM=dumb, want false")
	}

	t.Setenv("TERM", "xterm-256color")

	// Under `go test` stdout is a pipe, so this must still be false. If it ever
	// reports true here, headless daemons would attempt interactive OAuth.
	if isTerminal() {
		t.Error("isTerminal() = true with a non-TTY stdout, want false")
	}
}

// TestCheckCalendarNilConfig: the nil guard exists so callers need not
// pre-check; it must be a silent no-op rather than a panic.
func TestCheckCalendarNilConfig(t *testing.T) {
	t.Parallel()

	checkCalendar(t.Context(), nil)
}

// TestCheckCalendarDefersWithoutTerminal covers the headless first-run path:
// with no token file and no interactive terminal, Calendar must fall back to
// the queue-only stub so exams are preserved until OAuth is completed, rather
// than being enabled and failing every cycle.
// Not parallel: mutates package-level flag pointers.
func TestCheckCalendarDefersWithoutTerminal(t *testing.T) {
	setCalTokFile(t, filepath.Join(t.TempDir(), "absent_token.json"))

	cfg := config.TomlConfig{CalendarEnabled: true}
	checkCalendar(t.Context(), &cfg)

	if cfg.CalendarEnabled {
		t.Error("CalendarEnabled should be false without a token file on a non-interactive terminal")
	}

	if !cfg.CalendarDeferred {
		t.Error("CalendarDeferred should be true so exams queue until OAuth is completed")
	}
}

// TestCheckCalendarKeepsValidToken: a present, decodable token must leave
// Calendar live. Deferring here would silently stop delivering exams for a
// correctly configured user.
// Not parallel: mutates package-level flag pointers.
func TestCheckCalendarKeepsValidToken(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "calendar_token.json")
	if err := os.WriteFile(tokFile,
		[]byte(`{"access_token":"a","token_type":"Bearer","refresh_token":"r","expiry":"2099-01-01T00:00:00Z"}`),
		0o600); err != nil {
		t.Fatal(err)
	}

	setCalTokFile(t, tokFile)

	cfg := config.TomlConfig{CalendarEnabled: true}
	checkCalendar(t.Context(), &cfg)

	if !cfg.CalendarEnabled {
		t.Error("CalendarEnabled should stay true for a valid token file")
	}

	if cfg.CalendarDeferred {
		t.Error("CalendarDeferred should stay false for a valid token file")
	}
}

// TestCheckCalendarDefersOnUnstatableToken covers the third failure mode: the
// token path exists but cannot be stat'd (an unsearchable parent directory).
// Deferring beats failing confusingly later in the cycle.
// Not parallel: mutates package-level flag pointers.
func TestCheckCalendarDefersOnUnstatableToken(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not block stat")
	}

	dir := filepath.Join(t.TempDir(), "locked")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	tokFile := filepath.Join(dir, "calendar_token.json")
	if err := os.WriteFile(tokFile, []byte(`{"access_token":"a"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Drop search permission so stat on the child fails with EACCES.
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	setCalTokFile(t, tokFile)

	cfg := config.TomlConfig{CalendarEnabled: true}
	checkCalendar(t.Context(), &cfg)

	if cfg.CalendarEnabled {
		t.Error("CalendarEnabled should be false when the token file cannot be stat'd")
	}

	if !cfg.CalendarDeferred {
		t.Error("CalendarDeferred should be true so exams are preserved")
	}
}

// TestRunPollCycleKeepsDBOpenForMessengers: msgDedup finishes as soon as
// gradesScraped closes, but the messengers are still draining and writing
// undelivered messages. Closing the database on wgFilter alone fails those
// writes, and the messages are already dedup-flagged, so they are lost for good.
//
// The scrape stage is stubbed because the real one talks to the portal: with no
// events in flight nothing races the close and the bug is invisible.
// Not parallel: mutates package-level flag pointers and the scrapeStage seam.
func TestRunPollCycleKeepsDBOpenForMessengers(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)
	setRetries(t, 1)
	resetExitLatch(t)

	dbPath := setDBFile(t, "poll-db-lifetime.db")

	// Seed the database so it is not a first run: a fresh database seeds
	// silently and forwards nothing, which would leave no writes to race the
	// close.
	openExistingDB(t, dbPath).Close() //nolint:errcheck

	const events = 300

	origStage := scrapeStage
	scrapeStage = func(_ context.Context, wg *sync.WaitGroup, ch chan<- msgtypes.Message, _ config.TomlConfig, _ *userSet) {
		wg.Go(func() {
			for i := range events {
				ch <- msgtypes.Message{
					Code:     msgtypes.Exam,
					Username: "pero.peric",
					Subject:  fmt.Sprintf("exam-%03d", i),
				}
			}
		})
	}

	t.Cleanup(func() { scrapeStage = origStage })

	// CalendarDeferred is a queue-only sink: every exam it receives is written
	// to the database, so a premature close shows up as missing rows.
	runWithinTimeout(t, 60*time.Second, "runPollCycle", func() {
		// The user is configured: first-run seeding is per user, and an
		// unconfigured one would seed silently and forward nothing.
		runPollCycle(t.Context(), config.TomlConfig{CalendarDeferred: true, User: []config.User{{Username: "pero.peric"}}})
	})

	// Reopen: runPollCycle closed its own handle on the way out.
	eDB := openExistingDB(t, *dbFile)
	defer eDB.Close() //nolint:errcheck

	got := queue.FetchFailedMsgs(t.Context(), eDB, messenger.CalendarQueueName)
	if len(got) != events {
		t.Fatalf("queue holds %d of %d exams; the database must stay open until every messenger has finished writing, or already-flagged events are lost for good",
			len(got), events)
	}
}

// TestCheckCalendarStaysLiveOnceConsentSaved: interactive consent can succeed —
// the token is on disk — and the calendar lookup that follows still fail on a
// network blip. Deferring then parked exams in the stub's queue, which nothing
// reads, until a restart. With the token saved the runtime init retries every
// cycle without prompting, so Calendar must stay live.
// Not parallel: mutates package-level seams and flag pointers.
func TestCheckCalendarStaysLiveOnceConsentSaved(t *testing.T) {
	tokFile := filepath.Join(t.TempDir(), "calendar_token.json")
	setCalTokFile(t, tokFile)

	origTerm, origInit := interactiveTerminal, calendarInit

	t.Cleanup(func() { interactiveTerminal, calendarInit = origTerm, origInit })

	interactiveTerminal = func() bool { return true }

	for _, tt := range []struct {
		name      string
		saveToken bool
		wantLive  bool
	}{
		{name: "token saved, lookup failed", saveToken: true, wantLive: true},
		{name: "consent failed", saveToken: false, wantLive: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_ = os.Remove(tokFile)

			calendarInit = func(context.Context, string, string, bool) (*calendar.Service, string, error) {
				if tt.saveToken {
					if err := os.WriteFile(tokFile, []byte(`{"access_token":"a"}`), 0o600); err != nil {
						t.Fatal(err)
					}
				}

				return nil, "", errors.New("injected calendar lookup failure")
			}

			cfg := config.TomlConfig{CalendarEnabled: true}
			checkCalendar(t.Context(), &cfg)

			if cfg.CalendarEnabled != tt.wantLive || cfg.CalendarDeferred == tt.wantLive {
				t.Errorf("CalendarEnabled=%v CalendarDeferred=%v, want live=%v",
					cfg.CalendarEnabled, cfg.CalendarDeferred, tt.wantLive)
			}
		})
	}
}
