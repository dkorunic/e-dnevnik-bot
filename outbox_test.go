// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/messenger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/rs/zerolog"
)

// existingDB opens a database past its first run, so dedup forwards new events.
func existingDB(t *testing.T, path string) *sqlitedb.Edb {
	t.Helper()

	eDB, err := sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	if err := eDB.Put(t.Context(), []byte("row"), []byte{1}); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	_ = eDB.Close()

	if eDB, err = sqlitedb.New(t.Context(), path); err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	return eDB
}

func subjectsIn(t *testing.T, eDB *sqlitedb.Edb, queueName []byte) []string {
	t.Helper()

	var out []string
	for _, q := range queue.FetchFailedMsgs(t.Context(), eDB, queueName) {
		out = append(out, q.Msg.Subject)
	}

	return out
}

// Dedup flags before delivery and never re-fires, so a process killed in between
// (SIGKILL, OOM, power) lost the alert. Not delivering what dedup handed over is
// that kill; the next cycle's recovery must queue it, once, per messenger.
// Not parallel: mutates package-level flag pointers.
func TestOutboxRecoversEventsAfterHardKill(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	eDB := existingDB(t, filepath.Join(t.TempDir(), "kill.db.sqlite"))

	if out := runDedup(t, t.Context(), eDB, grade("killed")); len(out) != 1 {
		t.Fatalf("dedup forwarded %d messages, want 1", len(out))
	}

	cfg := config.TomlConfig{TelegramEnabled: true, MailEnabled: true}

	recoverOutbox(t.Context(), eDB, cfg)
	recoverOutbox(t.Context(), eDB, cfg)

	for _, q := range [][]byte{messenger.TelegramQueueName, messenger.MailQueueName} {
		if got := subjectsIn(t, eDB, q); len(got) != 1 || got[0] != "killed" {
			t.Errorf("queue %s = %v, want the event exactly once", q, got)
		}
	}

	if got := subjectsIn(t, eDB, messenger.DiscordQueueName); len(got) != 0 {
		t.Errorf("a messenger that is not enabled was queued: %v", got)
	}

	if got := subjectsIn(t, eDB, queue.OutboxKey); len(got) != 0 {
		t.Errorf("outbox after recovery = %v, want it empty", got)
	}
}

// Only what is forwarded belongs in the outbox: anything else would be alerted
// by a recovery although dedup chose not to.
// Not parallel: mutates package-level flag pointers.
func TestOutboxHoldsOnlyForwardedEvents(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	eDB := existingDB(t, filepath.Join(t.TempDir(), "unforwarded.db.sqlite"))

	reading := grade("reading")
	reading.Code = msgtypes.Reading

	runDedup(t, t.Context(), eDB, reading)

	if got := subjectsIn(t, eDB, queue.OutboxKey); len(got) != 0 {
		t.Errorf("outbox = %v, want a suppressed event left out", got)
	}
}

// A claimed digest week never sends again, so the digest is as exposed as an event.
func TestOutboxRecoversDigestAfterHardKill(t *testing.T) {
	t.Parallel()

	eDB := existingDB(t, filepath.Join(t.TempDir(), "digest.db.sqlite"))

	now := time.Date(2026, time.November, 8, 18, 30, 0, 0, time.UTC)
	exams := map[string][]msgtypes.Message{"u@skole.hr": {{
		Code:      msgtypes.Exam,
		Username:  "u@skole.hr",
		Subject:   "Matematika",
		Timestamp: time.Date(2026, time.November, 10, 0, 0, 0, 0, time.UTC),
	}}}

	gradesMsg := make(chan msgtypes.Message, 1)

	sendDigests(t.Context(), eDB, &digestRun{now: now, users: []string{"u@skole.hr"}, day: time.Sunday, hour: 18}, exams, gradesMsg)

	if len(gradesMsg) != 1 {
		t.Fatalf("digest handed over %d messages, want 1", len(gradesMsg))
	}

	if got := subjectsIn(t, eDB, queue.OutboxKey); len(got) != 1 {
		t.Errorf("outbox = %v, want the digest in it", got)
	}
}

// A finished cycle has delivered or queued everything, so a row left behind
// would alert everyone again next cycle.
// Not parallel: mutates package-level flag pointers and the scrapeStage seam.
func TestRunPollCycleClearsOutbox(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)
	resetExitLatch(t)

	path := setDBFile(t, "cycle-outbox.db")

	eDB, err := sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	if err := eDB.Put(t.Context(), seedMigratedKey, []byte{1}); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	if !putSeedMarker(t.Context(), eDB, "testuser") {
		t.Fatal("putSeedMarker() failed")
	}

	_ = eDB.Close()

	origStage := scrapeStage
	scrapeStage = func(_ context.Context, wg *sync.WaitGroup, ch chan<- msgtypes.Message, _ config.TomlConfig, _ *userSet) {
		wg.Go(func() { ch <- grade("delivered") })
	}

	t.Cleanup(func() { scrapeStage = origStage })

	cfg := config.TomlConfig{User: []config.User{{Username: "testuser"}}}

	runWithinTimeout(t, 60*time.Second, "runPollCycle", func() { runPollCycle(t.Context(), cfg) })

	eDB, err = sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	if found, _ := eDB.CheckAndFlagTTL(t.Context(), "testuser", "delivered", grade("delivered").Fields); !found {
		t.Fatal("the cycle did not process the event")
	}

	if got := subjectsIn(t, eDB, queue.OutboxKey); len(got) != 0 {
		t.Errorf("outbox after a finished cycle = %v, want it empty", got)
	}
}

// A user with no classes stays unseeded all summer, and the announcement used to
// repeat hourly; markSeeded's line is the one that reports the first run.
// Not parallel: swaps the global logger.
func TestMsgDedupDoesNotAnnounceUnfinishedFirstRun(t *testing.T) {
	var buf bytes.Buffer

	orig := logger.Logger
	logger.Logger = zerolog.New(&buf)

	t.Cleanup(func() { logger.Logger = orig })

	eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), "announce.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	failed := &userSet{}
	failed.add("a@skole.hr")

	var wg sync.WaitGroup

	scraped := make(chan msgtypes.Message)
	close(scraped)

	msgDedup(t.Context(), eDB, &wg, scraped, make(chan msgtypes.Message), nil, &seedRun{failed: failed, users: []string{"a@skole.hr"}})
	wg.Wait()

	if out := buf.String(); strings.Contains(out, "First run") {
		t.Errorf("log = %q, want no first-run line for a scrape that did not finish", out)
	}
}

// A read failure recovers nothing, so clearing the outbox afterwards would delete
// a kill's survivors unmoved, losing alerts dedup will never fire again. A
// cancelled context stands in for it: SIGTERM just after the database opens.
// Not parallel: mutates package-level flag pointers.
func TestOutboxSurvivesFailedRecovery(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	path := filepath.Join(t.TempDir(), "unread.db.sqlite")
	eDB := existingDB(t, path)

	if out := runDedup(t, t.Context(), eDB, grade("killed")); len(out) != 1 {
		t.Fatalf("dedup forwarded %d messages, want 1", len(out))
	}

	cfg := config.TomlConfig{TelegramEnabled: true}

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	recoverOutbox(cancelled, eDB, cfg)
	queue.ClearOutbox(cancelled, eDB)

	if got := subjectsIn(t, eDB, queue.OutboxKey); len(got) != 1 || got[0] != "killed" {
		t.Errorf("outbox after a failed recovery = %v, want the event kept", got)
	}

	// The next cycle's handle has no failure on record, and delivers it.
	closeCycleDB(eDB)

	next, err := sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	t.Cleanup(func() { _ = next.Close() })

	recoverOutbox(t.Context(), next, cfg)

	if got := subjectsIn(t, next, messenger.TelegramQueueName); len(got) != 1 || got[0] != "killed" {
		t.Errorf("queue after the next recovery = %v, want the event", got)
	}
}
