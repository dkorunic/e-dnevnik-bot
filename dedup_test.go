// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// setReadingList points the package-level flag at v for the duration of the
// test. Callers must not run in parallel — the pointer is process-global.
func setReadingList(t *testing.T, v bool) {
	t.Helper()

	orig := readingList
	readingList = &v

	t.Cleanup(func() { readingList = orig })
}

// runDedup feeds msgs through msgDedup against eDB and returns everything that
// made it out to the messenger side.
func runDedup(t *testing.T, ctx context.Context, eDB *sqlitedb.Edb, msgs ...msgtypes.Message) []msgtypes.Message {
	t.Helper()

	gradesScraped := make(chan msgtypes.Message, len(msgs)+1)
	for _, m := range msgs {
		gradesScraped <- m
	}

	close(gradesScraped)

	gradesMsg := make(chan msgtypes.Message, len(msgs)+1)

	var wg sync.WaitGroup

	msgDedup(ctx, eDB, &wg, gradesScraped, gradesMsg)
	wg.Wait()

	// msgDedup closes gradesMsg via defer, so this drain terminates. If it ever
	// stopped closing, msgSend's fan-out would hang on shutdown.
	var out []msgtypes.Message
	for m := range gradesMsg {
		out = append(out, m)
	}

	return out
}

// grade builds a Grade event dated today so the relevance filter never
// interferes with what these tests are actually measuring.
func grade(subject string) msgtypes.Message {
	return msgtypes.Message{
		Code:     msgtypes.Grade,
		Username: "testuser",
		Subject:  subject,
		Fields:   []string{time.Now().Format(formatHRDateOnly), "5"},
	}
}

// TestMsgDedupFirstRunSeedsSilently pins the first-install behaviour called out
// in CLAUDE.md: a brand-new database must flag every event it sees but forward
// none of them. Without this a fresh install would alert on the student's
// entire grade history at once. The second half is the half that is easy to
// break — the events must actually be *flagged* during the silent run, so the
// next run (with an existing database) stays quiet rather than replaying them.
// Not parallel: mutates package-level flag pointers.
func TestMsgDedupFirstRunSeedsSilently(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	path := filepath.Join(t.TempDir(), "firstrun.db")

	history := []msgtypes.Message{grade("Matematika"), grade("Fizika"), grade("Kemija")}

	eDB, err := sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	if eDB.Existing() {
		t.Fatal("a brand-new database reported Existing(); the first-run guard would not engage")
	}

	if got := runDedup(t, t.Context(), eDB, history...); len(got) != 0 {
		t.Errorf("first run forwarded %d alerts, want 0 — a fresh install must not flood the user with backlog", len(got))
	}

	if err := eDB.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	// Second run against the same, now-populated database.
	eDB, err = sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New() reopen failed: %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	if !eDB.Existing() {
		t.Fatal("the seeded database did not report Existing() on reopen")
	}

	if got := runDedup(t, t.Context(), eDB, history...); len(got) != 0 {
		t.Errorf("second run forwarded %d alerts, want 0 — the silent first run must still flag events, not merely skip them", len(got))
	}

	// A genuinely new event must now come through.
	got := runDedup(t, t.Context(), eDB, grade("Biologija"))
	if len(got) != 1 || got[0].Subject != "Biologija" {
		t.Fatalf("second run forwarded %+v, want the one new event", got)
	}
}

// TestMsgDedupSuppressesDuplicates covers the steady state: an event already
// flagged must never alert again, while distinct events are unaffected.
// Not parallel: mutates package-level flag pointers.
func TestMsgDedupSuppressesDuplicates(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	eDB := openExistingDB(t, filepath.Join(t.TempDir(), "dup.db"))
	defer eDB.Close() //nolint:errcheck

	// Same event twice in one batch: the first flags it, the second is a hit.
	got := runDedup(t, t.Context(), eDB, grade("Matematika"), grade("Matematika"))
	if len(got) != 1 {
		t.Fatalf("forwarded %d alerts for a repeated event, want 1", len(got))
	}

	// And again on a later cycle.
	if got := runDedup(t, t.Context(), eDB, grade("Matematika")); len(got) != 0 {
		t.Errorf("forwarded %d alerts for an already-flagged event, want 0", len(got))
	}

	if got := runDedup(t, t.Context(), eDB, grade("Fizika")); len(got) != 1 {
		t.Errorf("forwarded %d alerts for a distinct event, want 1", len(got))
	}
}

// TestMsgDedupReadingListGate covers the --readinglist flag. The subtle part is
// that Reading events are flagged even while suppressed: that is what stops the
// portal's entire reading list from arriving at once the first time a user
// enables the flag.
// Not parallel: mutates package-level flag pointers.
func TestMsgDedupReadingListGate(t *testing.T) {
	setRelevancePeriod(t, 0)

	reading := msgtypes.Message{
		Code:     msgtypes.Reading,
		Username: "testuser",
		Subject:  "Hrvatski jezik",
		Fields:   []string{"Zlatarovo zlato"},
	}

	t.Run("suppressed while the flag is off", func(t *testing.T) {
		setRelevancePeriod(t, 0)
		setReadingList(t, false)

		eDB := openExistingDB(t, filepath.Join(t.TempDir(), "reading-off.db"))
		defer eDB.Close() //nolint:errcheck

		if got := runDedup(t, t.Context(), eDB, reading); len(got) != 0 {
			t.Errorf("forwarded %d reading alerts with --readinglist off, want 0", len(got))
		}
	})

	t.Run("delivered while the flag is on", func(t *testing.T) {
		setRelevancePeriod(t, 0)
		setReadingList(t, true)

		eDB := openExistingDB(t, filepath.Join(t.TempDir(), "reading-on.db"))
		defer eDB.Close() //nolint:errcheck

		got := runDedup(t, t.Context(), eDB, reading)
		if len(got) != 1 || got[0].Code != msgtypes.Reading {
			t.Errorf("forwarded %+v with --readinglist on, want the reading event", got)
		}
	})

	t.Run("enabling the flag later does not replay the backlog", func(t *testing.T) {
		setRelevancePeriod(t, 0)

		eDB := openExistingDB(t, filepath.Join(t.TempDir(), "reading-flip.db"))
		defer eDB.Close() //nolint:errcheck

		// Cycles with the flag off must still flag the events.
		setReadingList(t, false)

		if got := runDedup(t, t.Context(), eDB, reading); len(got) != 0 {
			t.Fatalf("forwarded %d reading alerts with the flag off, want 0", len(got))
		}

		// Turning the flag on must not resurrect the already-seen entry.
		setReadingList(t, true)

		if got := runDedup(t, t.Context(), eDB, reading); len(got) != 0 {
			t.Errorf("forwarded %d alerts after enabling --readinglist, want 0 — suppressed reading events must still be flagged, or enabling the flag floods the user", len(got))
		}
	})
}

// TestMsgDedupCancelledContextDoesNotFlag pins the ordering in the loop's first
// guard: msgDedup bails *before* CheckAndFlagTTL. An event flagged but not
// forwarded is lost forever, whereas an unflagged one is simply re-scraped next
// run — so on shutdown the safe direction is to leave it unflagged.
// Not parallel: mutates package-level flag pointers.
func TestMsgDedupCancelledContextDoesNotFlag(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	eDB := openExistingDB(t, filepath.Join(t.TempDir(), "cancelled.db"))
	defer eDB.Close() //nolint:errcheck

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if got := runDedup(t, ctx, eDB, grade("Matematika")); len(got) != 0 {
		t.Fatalf("forwarded %d alerts on a cancelled context, want 0", len(got))
	}

	// The event must not have been flagged: a live run has to still deliver it.
	got := runDedup(t, t.Context(), eDB, grade("Matematika"))
	if len(got) != 1 {
		t.Errorf("forwarded %d alerts on the following live run, want 1 — an event dropped at shutdown must stay unflagged so it re-scrapes", len(got))
	}
}

// TestMsgDedupClosesOutputChannel: msgDedup closes gradesMsg in a defer so
// msgSend's fan-out loop unblocks. Without the close, shutdown hangs.
// Not parallel: mutates package-level flag pointers.
func TestMsgDedupClosesOutputChannel(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	eDB := openExistingDB(t, filepath.Join(t.TempDir(), "close.db"))
	defer eDB.Close() //nolint:errcheck

	gradesScraped := make(chan msgtypes.Message)
	close(gradesScraped)

	gradesMsg := make(chan msgtypes.Message, 1)

	var wg sync.WaitGroup

	msgDedup(t.Context(), eDB, &wg, gradesScraped, gradesMsg)
	wg.Wait()

	select {
	case _, open := <-gradesMsg:
		if open {
			t.Fatal("gradesMsg yielded a message; want it closed and empty")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("gradesMsg was never closed; msgSend's fan-out would block forever on shutdown")
	}
}

// TestMsgDedupRelevanceFilterAppliesAfterFlagging: a stale event is suppressed
// but must still be flagged, so that a later run with a wider relevance window
// does not suddenly replay it.
// Not parallel: mutates package-level flag pointers.
func TestMsgDedupRelevanceFilterAppliesAfterFlagging(t *testing.T) {
	setReadingList(t, false)

	eDB := openExistingDB(t, filepath.Join(t.TempDir(), "stale.db"))
	defer eDB.Close() //nolint:errcheck

	stale := msgtypes.Message{
		Code:      msgtypes.Exam,
		Username:  "testuser",
		Subject:   "Povijest",
		Timestamp: time.Now().Add(-90 * 24 * time.Hour),
	}

	setRelevancePeriod(t, 30*24*time.Hour)

	if got := runDedup(t, t.Context(), eDB, stale); len(got) != 0 {
		t.Fatalf("forwarded %d alerts for a stale exam, want 0", len(got))
	}

	// Widening the window must not resurrect it — it was flagged when skipped.
	setRelevancePeriod(t, 365*24*time.Hour)

	if got := runDedup(t, t.Context(), eDB, stale); len(got) != 0 {
		t.Errorf("forwarded %d alerts after widening the relevance window, want 0 — stale events must be flagged when suppressed", len(got))
	}
}
