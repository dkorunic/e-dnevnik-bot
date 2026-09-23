// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/messenger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
)

// msgSendTimeout bounds the wait for msgSend to unwind. The failure this guards
// is a deadlock (channels waited on before being closed), which without a
// ceiling would hang the whole test binary instead of failing one test.
const msgSendTimeout = 30 * time.Second

// waitOrFail waits for wg, failing rather than hanging if msgSend deadlocks.
func waitOrFail(t *testing.T, wg *sync.WaitGroup) {
	t.Helper()

	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(msgSendTimeout):
		t.Fatal("msgSend did not return after gradesMsg was closed — the deferred sequence must close every messenger channel BEFORE wgInner.Wait(), or each drain loop blocks forever")
	}
}

// setRelevancePeriod points the package-level flag at d for the duration of the
// test. Callers must not run in parallel — the pointer is process-global.
func setRelevancePeriod(t *testing.T, d time.Duration) {
	t.Helper()

	orig := relevancePeriod
	relevancePeriod = &d

	t.Cleanup(func() { relevancePeriod = orig })
}

// TestMsgSendNoMessengersDrains covers the degenerate fan-out: with every
// messenger disabled there are no sinks, so msgSend must still drain gradesMsg
// and unwind on close. A fan-out that blocked waiting for a sink would hang the
// pipeline for any user who has not configured a messenger yet.
func TestMsgSendNoMessengersDrains(t *testing.T) {
	t.Parallel()

	eDB := openExistingDB(t, t.TempDir()+"/msgsend-empty.db")
	defer eDB.Close() //nolint:errcheck

	gradesMsg := make(chan msgtypes.Message, 4)
	for i := range 4 {
		gradesMsg <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: fmt.Sprintf("s%d", i)}
	}

	close(gradesMsg)

	var wg sync.WaitGroup

	msgSend(t.Context(), eDB, &wg, gradesMsg, config.TomlConfig{})

	waitOrFail(t, &wg)
}

// TestMsgSendDeferredCalendarQueuesExams walks a real sink end to end: msgSend
// starts the queue-only Calendar stub, fans messages into its channel, and
// unwinds cleanly. Only exams may be queued — Calendar delivers nothing else,
// and queuing grades would replay them into the calendar once OAuth completes.
func TestMsgSendDeferredCalendarQueuesExams(t *testing.T) {
	t.Parallel()

	eDB := openExistingDB(t, t.TempDir()+"/msgsend-deferred.db")
	defer eDB.Close() //nolint:errcheck

	gradesMsg := make(chan msgtypes.Message, 8)
	gradesMsg <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "exam-1"}
	gradesMsg <- msgtypes.Message{Code: msgtypes.Grade, Username: "u", Subject: "grade-1"}
	gradesMsg <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "exam-2"}
	gradesMsg <- msgtypes.Message{Code: msgtypes.Reading, Username: "u", Subject: "reading-1"}
	close(gradesMsg)

	var wg sync.WaitGroup

	msgSend(t.Context(), eDB, &wg, gradesMsg, config.TomlConfig{CalendarDeferred: true})

	waitOrFail(t, &wg)

	got := queue.FetchFailedMsgs(t.Context(), eDB, messenger.CalendarQueueName)

	subjects := make(map[string]bool, len(got))
	for _, q := range got {
		subjects[q.Msg.Subject] = true
	}

	for _, want := range []string{"exam-1", "exam-2"} {
		if !subjects[want] {
			t.Errorf("exam %q was not queued; deferred Calendar must preserve exams until OAuth completes", want)
		}
	}

	for _, unwanted := range []string{"grade-1", "reading-1"} {
		if subjects[unwanted] {
			t.Errorf("non-exam %q was queued; Calendar delivers exams only", unwanted)
		}
	}

	if len(got) != 2 {
		t.Errorf("queued %d messages, want 2: %+v", len(got), subjects)
	}
}

// TestMsgSendNonBlockingFanOutLosesNothing exercises the overflow path. The
// producer only does channel sends while the deferred-Calendar sink does a
// SQLite write per message, so with far more messages than messengerBufLen the
// sink falls behind and the select hits its default branch. Whichever path each
// message takes — buffered hand-off or spill — it must end up in the same
// queue: the isolation trade-off is a late delivery, never a dropped alert.
func TestMsgSendNonBlockingFanOutLosesNothing(t *testing.T) {
	t.Parallel()

	const total = messengerBufLen * 3

	eDB := openExistingDB(t, t.TempDir()+"/msgsend-overflow.db")
	defer eDB.Close() //nolint:errcheck

	gradesMsg := make(chan msgtypes.Message, total)
	for i := range total {
		gradesMsg <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: fmt.Sprintf("exam-%03d", i)}
	}

	close(gradesMsg)

	var wg sync.WaitGroup

	msgSend(t.Context(), eDB, &wg, gradesMsg, config.TomlConfig{CalendarDeferred: true})

	waitOrFail(t, &wg)

	got := queue.FetchFailedMsgs(t.Context(), eDB, messenger.CalendarQueueName)

	seen := make(map[string]bool, len(got))
	for _, q := range got {
		seen[q.Msg.Subject] = true
	}

	for i := range total {
		want := fmt.Sprintf("exam-%03d", i)
		if !seen[want] {
			t.Fatalf("message %q reached neither the messenger nor its queue — the non-blocking fan-out must spill, not drop", want)
		}
	}
}

// TestMsgSendCancelledContextStillQueues checks the shutdown-tolerant write
// path through msgSend: with ctx already cancelled the queue write is detached
// (context.WithoutCancel), so a SIGTERM arriving mid-cycle must still persist
// already dedup-flagged events instead of losing them forever.
func TestMsgSendCancelledContextStillQueues(t *testing.T) {
	t.Parallel()

	eDB := openExistingDB(t, t.TempDir()+"/msgsend-cancelled.db")
	defer eDB.Close() //nolint:errcheck

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	gradesMsg := make(chan msgtypes.Message, 1)
	gradesMsg <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "exam-during-shutdown"}
	close(gradesMsg)

	var wg sync.WaitGroup

	msgSend(ctx, eDB, &wg, gradesMsg, config.TomlConfig{CalendarDeferred: true})

	waitOrFail(t, &wg)

	got := queue.FetchFailedMsgs(context.Background(), eDB, messenger.CalendarQueueName)
	if len(got) != 1 || got[0].Msg.Subject != "exam-during-shutdown" {
		t.Fatalf("FetchFailedMsgs = %+v, want the exam persisted despite a cancelled context", got)
	}
}

// TestFlagMessengerError covers the exit-code latch. A shutdown-induced
// cancellation is part of a normal stop and must not turn a clean SIGTERM into
// a non-zero exit; every other failure must latch.
// Not parallel: mutates the package-level exitWithError latch.
func TestFlagMessengerError(t *testing.T) {
	tests := []struct {
		name      string
		cancelled bool
		err       error
		wantExit  bool
	}{
		{
			name:     "genuine messenger failure latches",
			err:      errors.New("invalid bot token"),
			wantExit: true,
		},
		{
			name:      "cancellation during shutdown does not latch",
			cancelled: true,
			err:       context.Canceled,
			wantExit:  false,
		},
		{
			name:      "wrapped cancellation during shutdown does not latch",
			cancelled: true,
			err:       fmt.Errorf("sending message: %w", context.Canceled),
			wantExit:  false,
		},
		{
			name:      "non-cancellation error during shutdown still latches",
			cancelled: true,
			err:       errors.New("smtp: 535 authentication failed"),
			wantExit:  true,
		},
		{
			name:     "cancellation on a live context still latches",
			err:      context.Canceled,
			wantExit: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := exitWithError.Load()

			t.Cleanup(func() { exitWithError.Store(orig) })

			exitWithError.Store(false)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			if tt.cancelled {
				cancel()
			}

			flagMessengerError(ctx, ErrDiscord, tt.err)

			if got := exitWithError.Load(); got != tt.wantExit {
				t.Errorf("exitWithError = %v, want %v", got, tt.wantExit)
			}
		})
	}
}

// TestIsStaleEventDisabled: a zero or negative relevance period disables the
// filter entirely, so even a years-old event passes through.
// Not parallel: mutates the package-level relevancePeriod pointer.
func TestIsStaleEventDisabled(t *testing.T) {
	setRelevancePeriod(t, 0)

	old := msgtypes.Message{
		Code:      msgtypes.Exam,
		Timestamp: time.Now().Add(-5 * 365 * 24 * time.Hour),
	}

	if isStaleEvent(old, time.Now()) {
		t.Error("isStaleEvent = true with relevancePeriod 0; the filter must be disabled")
	}
}

// TestIsStaleEventExam covers the exam branch, which filters on the parsed ICS
// timestamp. A zero timestamp means the portal gave no date — fail open rather
// than silently drop the alert.
// Not parallel: mutates the package-level relevancePeriod pointer.
func TestIsStaleEventExam(t *testing.T) {
	setRelevancePeriod(t, 30*24*time.Hour)

	now := time.Now()

	tests := []struct {
		name      string
		timestamp time.Time
		want      bool
	}{
		{
			name:      "exam inside the window is fresh",
			timestamp: now.Add(-24 * time.Hour),
			want:      false,
		},
		{
			name:      "exam just inside the boundary is fresh",
			timestamp: now.Add(-29 * 24 * time.Hour),
			want:      false,
		},
		{
			name:      "exam outside the window is stale",
			timestamp: now.Add(-31 * 24 * time.Hour),
			want:      true,
		},
		{
			name:      "zero timestamp fails open",
			timestamp: time.Time{},
			want:      false,
		},
		{
			name:      "future exam is fresh",
			timestamp: now.Add(48 * time.Hour),
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "s", Timestamp: tt.timestamp}

			if got := isStaleEvent(g, now); got != tt.want {
				t.Errorf("isStaleEvent() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestIsStaleEventGradeFailsOpen: Fields[0] is assumed to be the grade date, but
// it comes from scraped HTML. An unparseable or absent value must fail open — a
// stale alert is recoverable, a silently dropped grade is not.
// Not parallel: mutates the package-level relevancePeriod pointer.
func TestIsStaleEventGradeFailsOpen(t *testing.T) {
	setRelevancePeriod(t, 24*time.Hour)

	tests := []struct {
		name   string
		fields []string
	}{
		{name: "no fields at all", fields: nil},
		{name: "empty first field", fields: []string{"", "5"}},
		{name: "non-date text", fields: []string{"nema datuma", "5"}},
		{name: "wrong separator", fields: []string{"15/04", "5"}},
		{name: "full year included", fields: []string{"15.4.2025.", "5"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := msgtypes.Message{Code: msgtypes.Grade, Username: "u", Subject: "s", Fields: tt.fields}

			if isStaleEvent(g, time.Now()) {
				t.Errorf("isStaleEvent() = true for fields %q; an unparseable date must fail open", tt.fields)
			}
		})
	}
}

// TestIsStaleEventUnfilteredCodes: only Exam and Grade are time-filtered.
// Reading, FinalGrade and NationalExam carry no usable date, so they must pass
// through regardless of how old the surrounding data looks.
// Not parallel: mutates the package-level relevancePeriod pointer.
func TestIsStaleEventUnfilteredCodes(t *testing.T) {
	setRelevancePeriod(t, time.Hour)

	longAgo := time.Now().Add(-365 * 24 * time.Hour)
	staleDate := longAgo.Format(formatHRDateOnly)

	for _, code := range []msgtypes.EventCode{msgtypes.Reading, msgtypes.FinalGrade, msgtypes.NationalExam} {
		g := msgtypes.Message{
			Code:      code,
			Username:  "u",
			Subject:   "s",
			Timestamp: longAgo,
			Fields:    []string{staleDate, "5"},
		}

		if isStaleEvent(g, time.Now()) {
			t.Errorf("isStaleEvent() = true for code %v; only Exam and Grade are time-filtered", code)
		}
	}
}

// TestGithubClient checks both construction paths. The token path must not fail
// on a syntactically valid token — a broken client here would silently disable
// the update check for every authenticated user.
// Not parallel: mutates the GITHUB_TOKEN environment variable.
func TestGithubClient(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")

	client, err := githubClient()
	if err != nil {
		t.Fatalf("githubClient() without token = %v, want nil", err)
	}

	if client == nil {
		t.Fatal("githubClient() without token returned nil client")
	}

	t.Setenv("GITHUB_TOKEN", "ghp_0123456789abcdefghijklmnopqrstuvwxyz")

	client, err = githubClient()
	if err != nil {
		t.Fatalf("githubClient() with token = %v, want nil", err)
	}

	if client == nil {
		t.Fatal("githubClient() with token returned nil client")
	}
}

// TestVersionCheckSkipsLocalBuilds pins the "don't phone home from a source
// build" rule: an empty GitTag or a dirty tree must return before any GitHub
// call. The observable signal is that it returns promptly without network.
// Not parallel: mutates the package-level build-info vars.
func TestVersionCheckSkipsLocalBuilds(t *testing.T) {
	origTag, origDirty := GitTag, GitDirty

	t.Cleanup(func() { GitTag, GitDirty = origTag, origDirty })

	tests := []struct {
		name  string
		tag   string
		dirty string
	}{
		{name: "untagged source build", tag: "", dirty: ""},
		{name: "dirty working tree", tag: "v1.2.3", dirty: "dirty"},
		{name: "unparseable tag", tag: "not-a-semver", dirty: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			GitTag, GitDirty = tt.tag, tt.dirty

			var wg sync.WaitGroup

			versionCheck(t.Context(), &wg)

			done := make(chan struct{})

			go func() {
				wg.Wait()
				close(done)
			}()

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("versionCheck did not return promptly; local/dirty builds must skip the GitHub call")
			}
		})
	}
}

// TestSpinnerStopsOnDone verifies the spinner honours its done channel. It
// selects on done alongside the rotate delay precisely so shutdown is not held
// by an in-flight sleep.
func TestSpinnerStopsOnDone(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	stopped := make(chan struct{})

	go func() {
		spinner(done)
		close(stopped)
	}()

	close(done)

	select {
	case <-stopped:
	case <-time.After(spinnerRotateDelay * 20):
		t.Fatal("spinner did not return after done was closed; it must select on done rather than sleep unconditionally")
	}
}

// TestMsgSendClosesEveryMessengerChannel: each messenger's range loop exits only
// when its own channel closes, so closing just the first sink leaves the rest
// blocked and wgInner.Wait() waiting forever. Invisible with one messenger
// configured — it would first appear for whoever enables a second backend.
//
// Discord gets an empty token on purpose: it drains to its queue and returns,
// giving a second real sink with no network I/O.
// Not parallel: mutates package-level flag pointers and the exit latch.
func TestMsgSendClosesEveryMessengerChannel(t *testing.T) {
	setRetries(t, 1)
	resetExitLatch(t)

	eDB := openExistingDB(t, t.TempDir()+"/msgsend-multi.db")
	defer eDB.Close() //nolint:errcheck

	gradesMsg := make(chan msgtypes.Message, 1)
	gradesMsg <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "multi-sink"}
	close(gradesMsg)

	var wg sync.WaitGroup

	msgSend(t.Context(), eDB, &wg, gradesMsg, config.TomlConfig{
		DiscordEnabled:   true,
		CalendarDeferred: true,
	})

	waitOrFail(t, &wg)

	for _, queueName := range [][]byte{messenger.DiscordQueueName, messenger.CalendarQueueName} {
		got := queue.FetchFailedMsgs(t.Context(), eDB, queueName)
		if len(got) != 1 || got[0].Msg.Subject != "multi-sink" {
			t.Errorf("queue %s = %+v, want the message delivered to every configured sink", queueName, got)
		}
	}
}

// TestDispatchSpillsInsteadOfBlocking pins the fan-out's isolation guarantee.
// Mail is limited to twenty sends an hour, so a burst leaves it minutes behind
// while the others idle; blocking on its channel would pace every messenger, and
// every later message, behind it.
//
// The channel is full and never drained, so a blocking send hangs here — the
// timeout is what turns that into a failure rather than a wedged test binary.
func TestDispatchSpillsInsteadOfBlocking(t *testing.T) {
	t.Parallel()

	eDB := openExistingDB(t, t.TempDir()+"/dispatch-spill.db")
	defer eDB.Close() //nolint:errcheck

	queueName := []byte("test-dispatch-spill-queue")

	// Capacity one, already occupied: the next send cannot proceed.
	s := messengerSink{ch: make(chan msgtypes.Message, 1), queue: queueName}
	s.ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "already-buffered"}

	done := make(chan struct{})

	go func() {
		defer close(done)

		dispatch(t.Context(), eDB, s, msgtypes.Message{
			Code: msgtypes.Exam, Username: "u", Subject: "spilled-not-blocked",
		})
	}()

	select {
	case <-done:
	case <-time.After(msgSendTimeout):
		t.Fatal("dispatch blocked on a full messenger buffer; a messenger that has fallen behind must have its message spilled to the queue, never pace the rest of the fan-out")
	}

	got := queue.FetchFailedMsgs(t.Context(), eDB, queueName)
	if len(got) != 1 || got[0].Msg.Subject != "spilled-not-blocked" {
		t.Fatalf("FetchFailedMsgs = %+v, want the overflowed message spilled to the messenger's queue", got)
	}

	// The buffered message must be untouched — a spill replaces neither.
	if first := <-s.ch; first.Subject != "already-buffered" {
		t.Errorf("buffered message = %q, want it left in place", first.Subject)
	}
}

// TestMsgSendDeferredCalendarOverflowDropsNonExams: the deferred stub only
// writes its queue, so a non-exam spilled there by the fan-out is unconsumable
// until MaxQueueAge. Traffic is grade-heavy with periodic exams — the exam
// writes are what slow the sink into the spill branch.
func TestMsgSendDeferredCalendarOverflowDropsNonExams(t *testing.T) {
	t.Parallel()

	const (
		total       = messengerBufLen * 3
		examEvery   = 10
		wantedExams = total / examEvery
	)

	eDB := openExistingDB(t, t.TempDir()+"/msgsend-overflow-nonexam.db")
	defer eDB.Close() //nolint:errcheck

	gradesMsg := make(chan msgtypes.Message, total)

	for i := range total {
		if i%examEvery == 0 {
			gradesMsg <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: fmt.Sprintf("exam-%03d", i)}

			continue
		}

		gradesMsg <- msgtypes.Message{Code: msgtypes.Grade, Username: "u", Subject: fmt.Sprintf("grade-%03d", i)}
	}

	close(gradesMsg)

	var wg sync.WaitGroup

	msgSend(t.Context(), eDB, &wg, gradesMsg, config.TomlConfig{CalendarDeferred: true})

	waitOrFail(t, &wg)

	got := queue.FetchFailedMsgs(t.Context(), eDB, messenger.CalendarQueueName)

	var spilledGrades []string

	for _, q := range got {
		if q.Msg.Code != msgtypes.Exam {
			spilledGrades = append(spilledGrades, q.Msg.Subject)
		}
	}

	if len(spilledGrades) > 0 {
		t.Errorf("overflow spilled %d non-exam message(s) into the Calendar queue; nothing reads that queue while Calendar is deferred, so these rows sit until MaxQueueAge: %v",
			len(spilledGrades), spilledGrades)
	}

	// Exams must still all survive — the filter must not become a general drop.
	if len(got)-len(spilledGrades) != wantedExams {
		t.Errorf("queued %d exams, want %d; the overflow filter must drop only what Calendar would discard",
			len(got)-len(spilledGrades), wantedExams)
	}
}

// TestDispatchSkipsUnqueueableOverflow pins the filter deterministically; the
// msgSend test above only reaches the spill branch by racing the fan-out
// against a slow sink.
func TestDispatchSkipsUnqueueableOverflow(t *testing.T) {
	t.Parallel()

	// The real queues, not an injected predicate: the rule lives with the
	// queue, so this exercises the wiring an operator actually gets.
	for _, q := range [][]byte{messenger.CalendarQueueName, messenger.CalDAVQueueName} {
		t.Run(string(q), func(t *testing.T) {
			t.Parallel()

			eDB := openExistingDB(t, t.TempDir()+"/dispatch-queueable.db")
			defer eDB.Close() //nolint:errcheck

			// Capacity one, already occupied: every further send takes the spill branch.
			s := messengerSink{
				ch:    make(chan msgtypes.Message, 1),
				queue: q,
			}
			s.ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "already-buffered"}

			dispatch(t.Context(), eDB, s, msgtypes.Message{Code: msgtypes.Grade, Username: "u", Subject: "dropped-grade"})

			if got := queue.FetchFailedMsgs(t.Context(), eDB, q); len(got) != 0 {
				t.Fatalf("FetchFailedMsgs = %+v, want nothing queued: this messenger discards non-exams, so the row would be unconsumable", got)
			}

			dispatch(t.Context(), eDB, s, msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "spilled-exam"})

			got := queue.FetchFailedMsgs(t.Context(), eDB, q)
			if len(got) != 1 || got[0].Msg.Subject != "spilled-exam" {
				t.Fatalf("FetchFailedMsgs = %+v, want only the exam spilled; the filter must not become a general drop", got)
			}
		})
	}
}

// TestIsStaleEventLeapDay: "29.2." only exists in a leap year, so attaching a
// non-leap year rolls it to 1 March and makes a grade from an earlier leap year
// look days old. The year has to walk back to one that actually has the date.
func TestIsStaleEventLeapDay(t *testing.T) {
	setRelevancePeriod(t, 30*24*time.Hour)

	// 2027 is not a leap year, so the most recent 29 February was 2024's.
	now := time.Date(2027, 3, 5, 12, 0, 0, 0, time.UTC)

	g := msgtypes.Message{
		Code:     msgtypes.Grade,
		Username: "u",
		Subject:  "Matematika",
		Fields:   []string{"29.2."},
	}

	if !isStaleEvent(g, now) {
		t.Error("a 29 February grade seen in March 2027 was treated as fresh; the nearest such date is 2024-02-29, years outside the window")
	}

	// A leap year must still resolve to the date itself, not walk past it.
	leapNow := time.Date(2028, 3, 5, 12, 0, 0, 0, time.UTC)
	if isStaleEvent(g, leapNow) {
		t.Error("a 29 February 2028 grade seen days later was treated as stale")
	}
}

// TestIsStaleEventBlankDateIsNotAnError: cellValues pads empty cells, so a
// subject whose first column is blank is padding, not portal drift. Logging it
// at Error emits one line per event per cycle, forever.
func TestIsStaleEventBlankDateIsNotAnError(t *testing.T) {
	setRelevancePeriod(t, 30*24*time.Hour)

	var buf bytes.Buffer

	orig := logger.Logger
	logger.Logger = logger.Output(&buf)

	t.Cleanup(func() { logger.Logger = orig })

	g := msgtypes.Message{
		Code:     msgtypes.Grade,
		Username: "u",
		Subject:  "Matematika",
		Fields:   []string{"", "5"},
	}

	if isStaleEvent(g, time.Now()) {
		t.Error("a blank date must fail open, not suppress the alert")
	}

	if got := buf.String(); strings.Contains(got, `"level":"error"`) {
		t.Errorf("a blank date logged at error level: %v", got)
	}

	// A non-empty value that genuinely will not parse must still be loud.
	buf.Reset()

	g.Fields = []string{"not-a-date"}

	if isStaleEvent(g, time.Now()) {
		t.Error("an unparseable date must fail open")
	}

	if got := buf.String(); !strings.Contains(got, `"level":"error"`) {
		t.Errorf("an unparseable date was not reported at error level: %v", got)
	}
}

// TestResolveHRYear covers the year the portal's "D.M." column omits. 29
// February is the only value absent from some years, and the gap spans a
// non-leap century boundary (1900, 2100), so the walk has to reach further back
// than four years.
func TestResolveHRYear(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		date string
		now  time.Time
		want time.Time
	}{
		{
			name: "already passed this year",
			date: "15.4.",
			now:  time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC),
			want: time.Date(2026, 4, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "still ahead implies last year",
			date: "20.9.",
			now:  time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC),
			want: time.Date(2025, 9, 20, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "leap day in a leap year is itself",
			date: "29.2.",
			now:  time.Date(2028, 3, 5, 0, 0, 0, 0, time.UTC),
			want: time.Date(2028, 2, 29, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "leap day in a common year walks back",
			date: "29.2.",
			now:  time.Date(2027, 3, 5, 0, 0, 0, 0, time.UTC),
			want: time.Date(2024, 2, 29, 0, 0, 0, 0, time.UTC),
		},
		{
			// 2100 is not a leap year, so 2096 is the nearest — seven steps,
			// which a four-year assumption cannot reach.
			name: "leap day across a non-leap century",
			date: "29.2.",
			now:  time.Date(2103, 3, 5, 0, 0, 0, 0, time.UTC),
			want: time.Date(2096, 2, 29, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			parsed, err := time.Parse(formatHRDateOnly, tt.date)
			if err != nil {
				t.Fatalf("time.Parse(%q) = %v", tt.date, err)
			}

			got, ok := resolveHRYear(parsed, tt.now)
			if !ok {
				t.Fatalf("resolveHRYear(%q, %v) reported no resolvable year", tt.date, tt.now)
			}

			if !got.Equal(tt.want) {
				t.Errorf("resolveHRYear(%q, %v) = %v, want %v", tt.date, tt.now, got, tt.want)
			}
		})
	}
}
