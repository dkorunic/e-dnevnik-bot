// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// newRunTestDB opens a throwaway database for the recipientRun tests.
func newRunTestDB(t *testing.T) *sqlitedb.Edb {
	t.Helper()

	eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), "run.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	return eDB
}

// runTestMsg is the message every case in this file drives through a run.
func runTestMsg() msgtypes.Message {
	return msgtypes.Message{
		Username:     "u@skole.hr",
		Subject:      "recipient-run",
		Descriptions: []string{"D"},
		Fields:       []string{"A"},
	}
}

// TestRecipientRunRequeueDecision is the table that pins recipientRun's whole
// reason for existing: when a message is written back to the queue and when it
// is not. Each row is an outcome combination a messenger can produce.
//
// The all-poisoned row is the one that costs real money to get wrong. A
// permanent failure requeued is re-attempted once per cycle until MaxQueueAge
// — a month of hammering an API that rate-limits and bans for abuse.
func TestRecipientRunRequeueDecision(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		outcome     func(r *recipientRun)
		wantQueued  bool
		wantSkipped []string
	}{
		{
			name:       "everything delivered",
			outcome:    func(r *recipientRun) { r.delivered("a", "b") },
			wantQueued: false,
		},
		{
			name:       "every recipient permanently failed",
			outcome:    func(r *recipientRun) { r.poison("a", "b") },
			wantQueued: false,
		},
		{
			name:       "nothing happened at all",
			outcome:    func(_ *recipientRun) {},
			wantQueued: false,
		},
		{
			name:        "one transient failure",
			outcome:     func(r *recipientRun) { r.delivered("a"); r.failed() },
			wantQueued:  true,
			wantSkipped: []string{"a"},
		},
		{
			name:        "transient failure alongside a poisoned recipient",
			outcome:     func(r *recipientRun) { r.delivered("a"); r.poison("b"); r.failed() },
			wantQueued:  true,
			wantSkipped: []string{"a", "b"},
		},
		{
			name:        "interrupted by shutdown with nothing failed",
			outcome:     func(r *recipientRun) { r.delivered("a"); r.interrupt() },
			wantQueued:  true,
			wantSkipped: []string{"a"},
		},
		{
			name:        "interrupted after poisoning",
			outcome:     func(r *recipientRun) { r.poison("a"); r.interrupt() },
			wantQueued:  true,
			wantSkipped: []string{"a"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eDB := newRunTestDB(t)
			g := runTestMsg()

			run := newRecipientRun(g)
			tt.outcome(run)
			run.finish(t.Context(), eDB, DiscordQueueName, g)

			got := queue.FetchFailedMsgs(t.Context(), eDB, DiscordQueueName)

			if !tt.wantQueued {
				if len(got) != 0 {
					t.Fatalf("queued %d message(s), want none — a message with nothing left to retry must not be rewritten", len(got))
				}

				return
			}

			if len(got) != 1 {
				t.Fatalf("queued %d message(s), want 1 — an unretried recipient is lost for good otherwise", len(got))
			}

			skipped := got[0].Msg.SkipRecipients
			slices.Sort(skipped)

			want := slices.Clone(tt.wantSkipped)
			slices.Sort(want)

			if !slices.Equal(skipped, want) {
				t.Errorf("SkipRecipients = %q, want %q — delivered recipients missing here get a duplicate, poisoned ones get retried forever",
					skipped, want)
			}
		})
	}
}

// TestRecipientRunSkipsAlreadyResolvedRecipients checks the skip set is seeded
// from the message, so a requeued message does not re-contact whoever an
// earlier attempt already reached.
func TestRecipientRunSkipsAlreadyResolvedRecipients(t *testing.T) {
	t.Parallel()

	g := runTestMsg()
	g.SkipRecipients = []string{"already", "done"}

	run := newRecipientRun(g)

	for _, id := range g.SkipRecipients {
		if !run.skipped(id) {
			t.Errorf("skipped(%q) = false, want true — the recipient would be sent a duplicate", id)
		}
	}

	if run.skipped("fresh") {
		t.Error("skipped(\"fresh\") = true, want false — an unattempted recipient would never be contacted")
	}
}

// TestRecipientRunMergesRatherThanReplacesSkipRecipients checks that a second
// attempt's outcome is added to the first attempt's set. Replacing it would
// re-contact everyone the earlier attempt reached.
func TestRecipientRunMergesRatherThanReplacesSkipRecipients(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)

	g := runTestMsg()
	g.SkipRecipients = []string{"first-round"}

	run := newRecipientRun(g)
	run.delivered("second-round")
	run.failed()
	run.finish(t.Context(), eDB, SlackQueueName, g)

	got := queue.FetchFailedMsgs(t.Context(), eDB, SlackQueueName)
	if len(got) != 1 {
		t.Fatalf("queued %d message(s), want 1", len(got))
	}

	skipped := got[0].Msg.SkipRecipients
	slices.Sort(skipped)

	if want := []string{"first-round", "second-round"}; !slices.Equal(skipped, want) {
		t.Errorf("SkipRecipients = %q, want %q", skipped, want)
	}
}

// TestRecipientRunDeduplicatesSkipRecipients checks repeated failures cannot
// grow the set without bound: a message re-failing for different subsets over
// many cycles must not accumulate the same ID once per cycle.
func TestRecipientRunDeduplicatesSkipRecipients(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)

	g := runTestMsg()
	g.SkipRecipients = []string{"a", "b"}

	run := newRecipientRun(g)
	run.delivered("a", "c")
	run.poison("b")
	run.failed()
	run.finish(t.Context(), eDB, SlackQueueName, g)

	got := queue.FetchFailedMsgs(t.Context(), eDB, SlackQueueName)
	if len(got) != 1 {
		t.Fatalf("queued %d message(s), want 1", len(got))
	}

	skipped := got[0].Msg.SkipRecipients
	slices.Sort(skipped)

	if want := []string{"a", "b", "c"}; !slices.Equal(skipped, want) {
		t.Errorf("SkipRecipients = %q, want %q — a repeated ID here grows the row every cycle", skipped, want)
	}
}

// TestRecipientRunFinishSurvivesCancelledContext is the shutdown-tolerance
// guard. SIGTERM arriving mid-send cancels the context before the queue write;
// if that write rode the cancelled context it would fail, and the event — which
// dedup has already flagged and will never re-scrape — would be gone.
func TestRecipientRunFinishSurvivesCancelledContext(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)
	g := runTestMsg()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	run := newRecipientRun(g)
	run.delivered("a")
	run.interrupt()
	run.finish(ctx, eDB, MailQueueName, g)

	if got := queue.FetchFailedMsgs(t.Context(), eDB, MailQueueName); len(got) != 1 {
		t.Fatalf("queued %d message(s) under a cancelled context, want 1 — the alert is lost for good otherwise", len(got))
	}
}

// TestRecipientRunFinishLeavesCallerMessageUntouched checks finish mutates only
// its own copy. The fan-out hands the same Message value to every messenger, so
// writing SkipRecipients through would leak one backend's delivery state into
// the others and silently suppress their sends.
func TestRecipientRunFinishLeavesCallerMessageUntouched(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)

	g := runTestMsg()
	g.SkipRecipients = []string{"original"}

	run := newRecipientRun(g)
	run.delivered("delivered-here")
	run.failed()
	run.finish(t.Context(), eDB, TelegramQueueName, g)

	if want := []string{"original"}; !slices.Equal(g.SkipRecipients, want) {
		t.Errorf("caller's SkipRecipients = %q, want %q — another messenger would skip recipients it never contacted",
			g.SkipRecipients, want)
	}
}

// TestRecipientRunResolvedDoesNotAliasAccumulators checks resolved() hands back
// a fresh slice. Returning one backed by r.successful would let a caller's
// append overwrite r.poisoned, silently dropping poisoned recipients from
// SkipRecipients and retrying them until MaxQueueAge.
func TestRecipientRunResolvedDoesNotAliasAccumulators(t *testing.T) {
	t.Parallel()

	run := newRecipientRun(runTestMsg())
	run.delivered("a")
	run.poison("p")

	resolved := run.resolved()

	// Force a write past the end of the returned slice's length.
	resolved = append(resolved, "overwrite") //nolint:staticcheck // the append is the probe
	_ = resolved

	if got := run.resolved(); !slices.Equal(got, []string{"a", "p"}) {
		t.Errorf("resolved() = %q after a caller appended, want [a p] — the accumulators were aliased", got)
	}
}
