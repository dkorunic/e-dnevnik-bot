// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/messenger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// guardTestDB opens a throwaway database for the panic-guard tests.
func guardTestDB(t *testing.T) *sqlitedb.Edb {
	t.Helper()

	eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), "guard.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	return eDB
}

// guardTestMsg is the event driven through the stages below.
func guardTestMsg(subject string) msgtypes.Message {
	return msgtypes.Message{
		Code:         msgtypes.Grade,
		Username:     "u@skole.hr",
		Subject:      subject,
		Descriptions: []string{"Ocjena"},
		Fields:       []string{"5"},
	}
}

// waitFor runs fn and reports whether it finished inside d. Every case here is
// really asking "does this stage still terminate", so a timeout is the failure
// being tested, not an incidental guard.
func waitFor(t *testing.T, d time.Duration, fn func()) bool {
	t.Helper()

	done := make(chan struct{})

	go func() {
		defer close(done)

		fn()
	}()

	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// swapDispatch points the fan-out's dispatch seam at fn for the test.
func swapDispatch(t *testing.T, fn func(context.Context, *sqlitedb.Edb, messengerSink, msgtypes.Message)) {
	t.Helper()

	orig := dispatchFn
	dispatchFn = fn

	t.Cleanup(func() { dispatchFn = orig })
}

// TestMsgSendPanicDoesNotKillTheProcess: a panic in the fan-out must be
// contained the way recoverMessenger contains one inside a backend. Without the
// guard the panic unwinds past msgSend's goroutine and takes the daemon — and
// every other messenger — down with it.
// Not parallel: swaps the package-level dispatch seam and exit latch.
func TestMsgSendPanicDoesNotKillTheProcess(t *testing.T) {
	resetExitLatch(t)
	setRetries(t, 1)

	eDB := guardTestDB(t)

	swapDispatch(t, func(_ context.Context, _ *sqlitedb.Edb, _ messengerSink, _ msgtypes.Message) {
		panic("fan-out exploded")
	})

	gradesMsg := make(chan msgtypes.Message, 4)
	gradesMsg <- guardTestMsg("boom")
	close(gradesMsg)

	var wgMsg sync.WaitGroup

	cfg := config.TomlConfig{
		SlackEnabled: true,
		Slack:        config.Slack{Token: "xoxb-test", ChatIDs: []string{"C12345678"}},
	}

	if !waitFor(t, 30*time.Second, func() {
		msgSend(t.Context(), eDB, &wgMsg, gradesMsg, cfg)
		wgMsg.Wait()
	}) {
		t.Fatal("msgSend did not return after a fan-out panic; the cycle is wedged")
	}

	if !exitWithError.Load() {
		t.Error("a fan-out panic did not latch the run as failed; the process would exit 0 having delivered nothing")
	}
}

// TestMsgSendPanicDrainsGradesMsg is the load-bearing half of the fan-out
// guard. msgDedup hands messages over with a blocking send, so a recover that
// simply returned would leave the dedup goroutine parked on that send forever:
// wgFilter.Wait() never returns and the daemon hangs silently, which is worse
// than the crash the guard replaced.
//
// The test therefore models the real producer — a blocking sender on an
// unbuffered channel — and asserts it gets to finish.
// Not parallel: swaps the package-level dispatch seam and exit latch.
func TestMsgSendPanicDrainsGradesMsg(t *testing.T) {
	resetExitLatch(t)
	setRetries(t, 1)

	eDB := guardTestDB(t)

	swapDispatch(t, func(_ context.Context, _ *sqlitedb.Edb, _ messengerSink, _ msgtypes.Message) {
		panic("fan-out exploded")
	})

	// Unbuffered: every send blocks until the fan-out reads it, exactly as
	// msgDedup's handoff does.
	gradesMsg := make(chan msgtypes.Message)

	producerDone := make(chan struct{})

	go func() {
		defer close(producerDone)
		defer close(gradesMsg)

		for _, s := range []string{"first", "second", "third"} {
			gradesMsg <- guardTestMsg(s)
		}
	}()

	var wgMsg sync.WaitGroup

	cfg := config.TomlConfig{
		SlackEnabled: true,
		Slack:        config.Slack{Token: "xoxb-test", ChatIDs: []string{"C12345678"}},
	}

	msgSend(t.Context(), eDB, &wgMsg, gradesMsg, cfg)

	if !waitFor(t, 30*time.Second, func() { wgMsg.Wait() }) {
		t.Fatal("msgSend never finished after the panic")
	}

	select {
	case <-producerDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the producer is still blocked on its send; a real msgDedup would hang here and wedge the whole cycle")
	}
}

// TestMsgSendPanicSpillsToQueue: the drained messages are already dedup-flagged
// and will never be re-scraped, so they must reach the messenger's queue rather
// than the floor.
// Not parallel: swaps the package-level dispatch seam and exit latch.
func TestMsgSendPanicSpillsToQueue(t *testing.T) {
	resetExitLatch(t)
	setRetries(t, 1)

	eDB := guardTestDB(t)

	// Panic only on the first message, so the rest reach the recovery drain.
	var once sync.Once

	swapDispatch(t, func(_ context.Context, _ *sqlitedb.Edb, _ messengerSink, _ msgtypes.Message) {
		once.Do(func() { panic("fan-out exploded") })
	})

	gradesMsg := make(chan msgtypes.Message, 4)
	for _, s := range []string{"first", "second", "third"} {
		gradesMsg <- guardTestMsg(s)
	}

	close(gradesMsg)

	var wgMsg sync.WaitGroup

	cfg := config.TomlConfig{
		SlackEnabled: true,
		Slack:        config.Slack{Token: "xoxb-test", ChatIDs: []string{"C12345678"}},
	}

	msgSend(t.Context(), eDB, &wgMsg, gradesMsg, cfg)
	wgMsg.Wait()

	got := queue.FetchFailedMsgs(t.Context(), eDB, messenger.SlackQueueName)
	if len(got) == 0 {
		t.Fatal("nothing was spilled to the Slack queue after the fan-out panicked; already-flagged alerts were dropped")
	}
}

// TestMsgDedupPanicDoesNotKillTheProcess: the dedup stage gets the same
// containment as the fan-out, and must still close gradesMsg so the fan-out
// downstream of it unblocks.
// Not parallel: mutates the package-level exit latch.
func TestMsgDedupPanicDoesNotKillTheProcess(t *testing.T) {
	resetExitLatch(t)

	gradesScraped := make(chan msgtypes.Message, 4)
	gradesMsg := make(chan msgtypes.Message, 4)

	close(gradesScraped)

	var wgFilter sync.WaitGroup

	// A nil database panics at the first eDB.Existing() call, standing in for
	// any unexpected fault inside the stage.
	if !waitFor(t, 30*time.Second, func() {
		msgDedup(t.Context(), nil, &wgFilter, gradesScraped, gradesMsg)
		wgFilter.Wait()
	}) {
		t.Fatal("msgDedup did not return after a panic; the cycle is wedged")
	}

	if _, open := <-gradesMsg; open {
		t.Error("gradesMsg carries a message after a dedup panic; nothing should have been forwarded")
	}

	// A closed channel reads as not-open; an unclosed one would have blocked
	// the receive above until the test timed out.
	if !exitWithError.Load() {
		t.Error("a dedup panic did not latch the run as failed")
	}
}

// TestMsgDedupPanicDrainsGradesScraped is the dedup guard's load-bearing half.
// The scrapers hand events over with a send that only yields to ctx.Done(),
// which a panic never triggers, so a stage that stops reading parks every
// scraper on a full channel and wgScrape.Wait() never returns.
// Not parallel: mutates the package-level exit latch.
func TestMsgDedupPanicDrainsGradesScraped(t *testing.T) {
	resetExitLatch(t)

	// Unbuffered: the producer blocks until the stage reads, as a scraper does.
	gradesScraped := make(chan msgtypes.Message)
	gradesMsg := make(chan msgtypes.Message, 8)

	producerDone := make(chan struct{})

	go func() {
		defer close(producerDone)
		defer close(gradesScraped)

		for _, s := range []string{"first", "second", "third"} {
			gradesScraped <- guardTestMsg(s)
		}
	}()

	var wgFilter sync.WaitGroup

	msgDedup(t.Context(), nil, &wgFilter, gradesScraped, gradesMsg)

	if !waitFor(t, 30*time.Second, func() { wgFilter.Wait() }) {
		t.Fatal("msgDedup never finished after the panic")
	}

	select {
	case <-producerDone:
	case <-time.After(30 * time.Second):
		t.Fatal("the producer is still blocked on its send; a real scraper would hang here and wgScrape.Wait() would never return")
	}
}

// TestSpillAllContainsItsOwnPanic: the recovery drain must complete even when
// spilling fails, because completing it is what prevents the deadlock. A nil
// database makes every store panic.
func TestSpillAllContainsItsOwnPanic(t *testing.T) {
	t.Parallel()

	sinks := []messengerSink{{ch: make(chan msgtypes.Message, 1), queue: messenger.SlackQueueName}}

	if !waitFor(t, 30*time.Second, func() {
		spillAll(t.Context(), nil, sinks, guardTestMsg("boom"))
	}) {
		t.Fatal("spillAll did not return")
	}
}

// TestRecoverDedupForwardsFlaggedEvent exercises the guard directly: given an
// already-flagged in-flight event, it must reach gradesMsg rather than be
// discarded with the unflagged backlog.
func TestRecoverDedupForwardsFlaggedEvent(t *testing.T) {
	t.Parallel()

	resetExitLatch(t)

	gradesScraped := make(chan msgtypes.Message, 4)
	gradesScraped <- guardTestMsg("unflagged-backlog")
	close(gradesScraped)

	gradesMsg := make(chan msgtypes.Message, 4)

	flagged := guardTestMsg("already-flagged")

	if !waitFor(t, 30*time.Second, func() {
		recoverDedup(gradesScraped, gradesMsg, &flagged, "boom")
	}) {
		t.Fatal("recoverDedup did not return")
	}

	close(gradesMsg)

	var forwarded []msgtypes.Message
	for m := range gradesMsg {
		forwarded = append(forwarded, m)
	}

	if len(forwarded) != 1 {
		t.Fatalf("forwarded %d events, want exactly the flagged one: %+v", len(forwarded), forwarded)
	}

	if forwarded[0].Subject != "already-flagged" {
		t.Errorf("forwarded %q, want the already-flagged event; the unflagged backlog must be discarded for re-scraping",
			forwarded[0].Subject)
	}
}

// TestRecoverDedupForwardsNothingWhenNoEventWasFlagged: a panic before
// CheckAndFlagTTL leaves nothing committed, so nothing may be forwarded —
// forwarding an unflagged event would alert on it now and again next cycle.
func TestRecoverDedupForwardsNothingWhenNoEventWasFlagged(t *testing.T) {
	t.Parallel()

	resetExitLatch(t)

	gradesScraped := make(chan msgtypes.Message, 4)
	gradesScraped <- guardTestMsg("unflagged")
	close(gradesScraped)

	gradesMsg := make(chan msgtypes.Message, 4)

	recoverDedup(gradesScraped, gradesMsg, nil, "boom")

	close(gradesMsg)

	if m, open := <-gradesMsg; open {
		t.Errorf("forwarded %+v with nothing flagged; it would alert now and again next cycle", m)
	}
}

// TestMsgDedupPanicInWindowForwardsFlaggedEvent covers the loop instrumentation
// that recordsimagine the in-flight event, end to end.
//
// CheckAndFlagTTL commits the "seen" row, then ~30 lines of suppression checks
// run before the forward. A panic in that window leaves the event flagged but
// undelivered, and dedup never re-fires a flagged event — so the alert is gone
// on this cycle and every cycle after.
//
// The panic is raised the way it would really happen: *readingList is a
// package-level flag pointer dereferenced inside that window, so a nil one
// faults exactly between the flag and the send.
// Not parallel: nils a package-level flag pointer and the exit latch.
func TestMsgDedupPanicInWindowForwardsFlaggedEvent(t *testing.T) {
	resetExitLatch(t)

	dbPath := filepath.Join(t.TempDir(), "dedup.db.sqlite")

	// Seed and reopen so Existing() reports true; a fresh database seeds
	// silently and would forward nothing regardless.
	seed, err := sqlitedb.New(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	if _, err := seed.CheckAndFlagTTL(t.Context(), "seed", "seed", []string{"seed"}); err != nil {
		t.Fatalf("seeding failed: %v", err)
	}

	if err := seed.Close(); err != nil {
		t.Fatalf("closing the seed database failed: %v", err)
	}

	eDB, err := sqlitedb.New(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("reopening failed: %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	if !eDB.Existing() {
		t.Fatal("reopened database does not report Existing(); the test would prove nothing")
	}

	// Faults inside the flag-to-forward window.
	origReadingList := readingList
	readingList = nil

	t.Cleanup(func() { readingList = origReadingList })

	gradesScraped := make(chan msgtypes.Message, 4)
	gradesScraped <- guardTestMsg("flagged-then-interrupted")
	close(gradesScraped)

	gradesMsg := make(chan msgtypes.Message, 4)

	var wgFilter sync.WaitGroup

	msgDedup(t.Context(), eDB, &wgFilter, gradesScraped, gradesMsg)

	if !waitFor(t, 30*time.Second, func() { wgFilter.Wait() }) {
		t.Fatal("msgDedup did not finish after panicking mid-window")
	}

	var forwarded []msgtypes.Message
	for m := range gradesMsg {
		forwarded = append(forwarded, m)
	}

	if len(forwarded) != 1 || forwarded[0].Subject != "flagged-then-interrupted" {
		t.Fatalf("forwarded %+v, want the flagged event; it is already marked seen, so dropping it loses the alert permanently",
			forwarded)
	}
}
