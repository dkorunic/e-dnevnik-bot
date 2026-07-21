// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// TestRecoverMessengerDrainsToQueue: a recovered send-path panic drains the
// remaining messages to the queue and returns a wrapped ErrMessengerPanic,
// rather than crashing the process.
func TestRecoverMessengerDrainsToQueue(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	eDB, err := sqlitedb.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer eDB.Close() //nolint:errcheck

	// One message is consumed before the panic; the recover must drain the rest.
	ch := make(chan msgtypes.Message, 3)
	ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "consumed"}
	ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "queued1"}
	ch <- msgtypes.Message{Code: msgtypes.Grade, Username: "u", Subject: "queued2"}
	close(ch)

	gotErr := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = recoverMessenger(ctx, eDB, DiscordQueueName, ch, r, nil)
			}
		}()

		<-ch // consume first, then panic mid-send

		panic("simulated send-path panic")
	}()

	if !errors.Is(gotErr, ErrMessengerPanic) {
		t.Fatalf("expected wrapped ErrMessengerPanic, got %v", gotErr)
	}

	queued := queue.FetchFailedMsgs(ctx, eDB, DiscordQueueName)
	if len(queued) != 2 {
		t.Fatalf("expected 2 drained messages in queue, got %d", len(queued))
	}
}

// TestRecoverMessengerRequeuesInflight: the message consumed just before the
// panic (in neither ch nor the queue) must be requeued alongside the drained
// remainder, or it is lost forever (dedup never re-fires it).
func TestRecoverMessengerRequeuesInflight(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	eDB, err := sqlitedb.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer eDB.Close() //nolint:errcheck

	ch := make(chan msgtypes.Message, 1)
	ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "queued1"}
	close(ch)

	inflightMsg := msgtypes.Message{Code: msgtypes.Grade, Username: "u", Subject: "inflight"}

	gotErr := func() (err error) {
		inflight := &inflightMsg

		defer func() {
			if r := recover(); r != nil {
				err = recoverMessenger(ctx, eDB, DiscordQueueName, ch, r, inflight)
			}
		}()

		panic("simulated send-path panic")
	}()

	if !errors.Is(gotErr, ErrMessengerPanic) {
		t.Fatalf("expected wrapped ErrMessengerPanic, got %v", gotErr)
	}

	queued := queue.FetchFailedMsgs(ctx, eDB, DiscordQueueName)
	if len(queued) != 2 {
		t.Fatalf("expected inflight + 1 drained message in queue, got %d", len(queued))
	}

	found := false

	for _, q := range queued {
		if q.Msg.Subject == "inflight" {
			found = true
		}
	}

	if !found {
		t.Errorf("in-flight message missing from queue after panic: %+v", queued)
	}
}

// TestCalendarDeferredQueuesOnlyExams: the deferred stub queues exam events
// (dropping non-exams) so they reach the calendar once OAuth completes.
func TestCalendarDeferredQueuesOnlyExams(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	eDB, err := sqlitedb.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer eDB.Close() //nolint:errcheck

	ch := make(chan msgtypes.Message, 3)
	ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "exam1"}
	ch <- msgtypes.Message{Code: msgtypes.Grade, Username: "u", Subject: "grade-dropped"}
	ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "exam2"}
	close(ch)

	CalendarDeferred(ctx, eDB, ch)

	queued := queue.FetchFailedMsgs(ctx, eDB, CalendarQueueName)
	if len(queued) != 2 {
		t.Fatalf("expected 2 queued exam events, got %d", len(queued))
	}

	for _, q := range queued {
		if q.Msg.Code != msgtypes.Exam {
			t.Errorf("non-exam event leaked into calendar queue: %+v", q.Msg)
		}
	}
}

// TestCalendarDeferredQueuesUnderCancelledCtx: the shutdown-tolerant queue
// write (queueStoreCtx) must still land when the parent ctx is already
// cancelled — the exact situation on SIGTERM mid-drain.
func TestCalendarDeferredQueuesUnderCancelledCtx(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	eDB, err := sqlitedb.New(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	defer eDB.Close() //nolint:errcheck

	cancel()

	ch := make(chan msgtypes.Message, 1)
	ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "exam1"}
	close(ch)

	CalendarDeferred(ctx, eDB, ch)

	queued := queue.FetchFailedMsgs(context.Background(), eDB, CalendarQueueName)
	if len(queued) != 1 {
		t.Fatalf("expected 1 queued exam under cancelled ctx, got %d", len(queued))
	}
}
