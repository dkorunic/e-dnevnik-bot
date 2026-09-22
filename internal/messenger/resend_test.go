// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"slices"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// seedQueue writes subjects into queueName as separate rows, oldest first.
func seedQueue(t *testing.T, eDB *sqlitedb.Edb, queueName []byte, subjects ...string) {
	t.Helper()

	for _, s := range subjects {
		msg := msgtypes.Message{
			Username:     "u@skole.hr",
			Subject:      s,
			Descriptions: []string{"D"},
			Fields:       []string{"A"},
		}

		if err := queue.StoreFailedMsgs(t.Context(), eDB, queueName, msg); err != nil {
			t.Fatalf("StoreFailedMsgs(%q) failed: %v", s, err)
		}
	}
}

// queuedSubjects returns the subjects currently sitting in queueName.
func queuedSubjects(t *testing.T, eDB *sqlitedb.Edb, queueName []byte) []string {
	t.Helper()

	var out []string
	for _, q := range queue.FetchFailedMsgs(t.Context(), eDB, queueName) {
		out = append(out, q.Msg.Subject)
	}

	return out
}

// TestResendQueuedDrainsAndDequeues covers the happy path: every queued row is
// handed to process exactly once, and the queue is empty afterwards. A row left
// behind is redelivered next cycle; a row never processed is an alert the user
// never sees.
func TestResendQueuedDrainsAndDequeues(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)
	seedQueue(t, eDB, DiscordQueueName, "first", "second", "third")

	var seen []string

	resendQueued(t.Context(), eDB, DiscordQueueName, func(m msgtypes.Message) {
		seen = append(seen, m.Subject)
	})

	if want := []string{"first", "second", "third"}; !slices.Equal(seen, want) {
		t.Errorf("processed %q, want %q in queue order", seen, want)
	}

	if left := queuedSubjects(t, eDB, DiscordQueueName); len(left) != 0 {
		t.Errorf("queue still holds %q after a clean drain; those get redelivered every cycle", left)
	}
}

// TestResendQueuedProcessesBeforeDequeuing is the crash-safety ordering guard.
// The row must still exist while process runs: if it were dequeued first, a
// crash mid-send would lose an alert that dedup has already flagged and will
// never re-scrape. At-least-once is the deliberate choice here — a duplicate is
// recoverable, a drop is not.
func TestResendQueuedProcessesBeforeDequeuing(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)
	seedQueue(t, eDB, SlackQueueName, "in-flight")

	var presentDuringProcess bool

	resendQueued(t.Context(), eDB, SlackQueueName, func(_ msgtypes.Message) {
		// Observed from inside process: the row backing this message must
		// still be on disk, so a crash right here redelivers it.
		presentDuringProcess = len(queuedSubjects(t, eDB, SlackQueueName)) == 1
	})

	if !presentDuringProcess {
		t.Error("the row was dequeued before process ran; a crash mid-send would lose the alert permanently")
	}

	if left := queuedSubjects(t, eDB, SlackQueueName); len(left) != 0 {
		t.Errorf("row not dequeued after processing: %q", left)
	}
}

// TestResendQueuedStopsOnCancelledContext checks shutdown leaves the untouched
// rows alone. Processing them into a cancelled context cannot send anything, so
// draining on would burn the rows for nothing.
func TestResendQueuedStopsOnCancelledContext(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)
	seedQueue(t, eDB, TelegramQueueName, "a", "b", "c")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	processed := 0

	resendQueued(ctx, eDB, TelegramQueueName, func(_ msgtypes.Message) { processed++ })

	if processed != 0 {
		t.Errorf("processed %d message(s) under a cancelled context, want 0", processed)
	}

	if left := queuedSubjects(t, eDB, TelegramQueueName); len(left) != 3 {
		t.Errorf("queue holds %q after a cancelled drain, want all three rows kept for the next cycle", left)
	}
}

// TestResendQueuedStopsMidDrainOnCancellation checks cancellation arriving
// partway through stops the loop, leaving the unprocessed remainder queued
// while the already-handled rows stay dequeued.
func TestResendQueuedStopsMidDrainOnCancellation(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)
	seedQueue(t, eDB, MailQueueName, "a", "b", "c")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	processed := 0

	resendQueued(ctx, eDB, MailQueueName, func(_ msgtypes.Message) {
		processed++

		// Shutdown lands while the first message is being handled.
		cancel()
	})

	if processed != 1 {
		t.Errorf("processed %d message(s), want 1 before the cancellation took effect", processed)
	}

	left := queuedSubjects(t, eDB, MailQueueName)
	if want := []string{"b", "c"}; !slices.Equal(left, want) {
		t.Errorf("queue holds %q, want %q — the handled row dequeued, the rest kept", left, want)
	}
}

// TestResendQueuedDequeuesDespiteCancellation checks the dequeue of an
// already-processed row survives a context cancelled during process. Leaving it
// behind would redeliver a message that was just sent.
func TestResendQueuedDequeuesDespiteCancellation(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)
	seedQueue(t, eDB, WhatsAppQueueName, "only")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	resendQueued(ctx, eDB, WhatsAppQueueName, func(_ msgtypes.Message) { cancel() })

	if left := queuedSubjects(t, eDB, WhatsAppQueueName); len(left) != 0 {
		t.Errorf("queue holds %q after processing under a cancelled context; the recipient gets a duplicate next cycle", left)
	}
}

// TestResendQueuedOnEmptyQueueDoesNothing checks the common case — nothing
// queued — costs no process calls.
func TestResendQueuedOnEmptyQueueDoesNothing(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)

	called := 0

	resendQueued(t.Context(), eDB, CalendarQueueName, func(_ msgtypes.Message) { called++ })

	if called != 0 {
		t.Errorf("process called %d time(s) on an empty queue, want 0", called)
	}
}

// TestResendQueuedIsolatesQueues checks the prefix scan does not cross queue
// boundaries: draining one messenger's queue must not consume another's.
func TestResendQueuedIsolatesQueues(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)
	seedQueue(t, eDB, DiscordQueueName, "discord-row")
	seedQueue(t, eDB, SlackQueueName, "slack-row")

	var seen []string

	resendQueued(t.Context(), eDB, DiscordQueueName, func(m msgtypes.Message) {
		seen = append(seen, m.Subject)
	})

	if want := []string{"discord-row"}; !slices.Equal(seen, want) {
		t.Errorf("processed %q, want %q — one messenger consumed another's queue", seen, want)
	}

	if left := queuedSubjects(t, eDB, SlackQueueName); len(left) != 1 {
		t.Errorf("Slack queue = %q, want its row untouched", left)
	}
}

// TestResendQueuedKeepsRequeuedFailureAsNewRow pins the interaction with
// recipientRun.finish: a process that re-queues its own failure writes a fresh
// row, and dequeuing the original must not remove it. Getting this backwards
// silently drops every message that fails on a retry.
func TestResendQueuedKeepsRequeuedFailureAsNewRow(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)
	seedQueue(t, eDB, SlackQueueName, "fails-again")

	resendQueued(t.Context(), eDB, SlackQueueName, func(m msgtypes.Message) {
		// Stand in for a messenger whose send failed transiently.
		run := newRecipientRun(m)
		run.failed()
		run.finish(t.Context(), eDB, SlackQueueName, m)
	})

	left := queuedSubjects(t, eDB, SlackQueueName)
	if want := []string{"fails-again"}; !slices.Equal(left, want) {
		t.Fatalf("queue = %q, want exactly one fresh row: the retry must survive the original's dequeue", left)
	}
}
