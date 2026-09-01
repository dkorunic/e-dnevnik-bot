// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// newEntryTestDB opens a throwaway database for the messenger entry-point tests.
func newEntryTestDB(t *testing.T) *sqlitedb.Edb {
	t.Helper()

	eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), "messenger.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	return eDB
}

// TestMessengerMisconfigurationQueuesInsteadOfDropping is the central
// data-loss guard for every messenger entry point. By the time a message
// reaches a messenger it has already been flagged in the dedup store, so it
// will never be re-scraped: if a misconfigured messenger simply returned, the
// alert would be lost permanently. Each entry point must therefore drain its
// channel into its own queue *before* returning the configuration error.
//
// Not parallel: the entry points touch package-level client globals.
func TestMessengerMisconfigurationQueuesInsteadOfDropping(t *testing.T) {
	tests := []struct {
		name    string
		queue   []byte
		run     func(context.Context, *sqlitedb.Edb, <-chan msgtypes.Message) error
		wantErr error
	}{
		{
			name:  "Discord without a token",
			queue: DiscordQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message) error {
				return Discord(ctx, eDB, ch, DiscordConfig{UserIDs: []string{"123"}, Retries: 1})
			},
			wantErr: ErrDiscordEmptyAPIKey,
		},
		{
			name:  "Discord without recipients",
			queue: DiscordQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message) error {
				return Discord(ctx, eDB, ch, DiscordConfig{Token: "tok", Retries: 1})
			},
			wantErr: ErrDiscordEmptyUserIDs,
		},
		{
			name:  "Telegram without a token",
			queue: TelegramQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message) error {
				return Telegram(ctx, eDB, ch, TelegramConfig{ChatIDs: []string{"123"}, Retries: 1})
			},
			wantErr: ErrTelegramEmptyAPIKey,
		},
		{
			name:  "Telegram without recipients",
			queue: TelegramQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message) error {
				return Telegram(ctx, eDB, ch, TelegramConfig{Token: "tok", Retries: 1})
			},
			wantErr: ErrTelegramEmptyUserIDs,
		},
		{
			name:  "Slack without a token",
			queue: SlackQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message) error {
				return Slack(ctx, eDB, ch, SlackConfig{ChatIDs: []string{"C123"}, Retries: 1})
			},
			wantErr: ErrSlackEmptyAPIKey,
		},
		{
			name:  "Slack without recipients",
			queue: SlackQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message) error {
				return Slack(ctx, eDB, ch, SlackConfig{Token: "xoxb-test", Retries: 1})
			},
			wantErr: ErrSlackEmptyUserIDs,
		},
		{
			name:  "WhatsApp without recipients or groups",
			queue: WhatsAppQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message) error {
				return WhatsApp(ctx, eDB, ch, WhatsAppConfig{Retries: 1})
			},
			wantErr: ErrWhatsAppEmptyUserIDs,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eDB := newEntryTestDB(t)

			ch := make(chan msgtypes.Message, 3)
			ch <- msgtypes.Message{Code: msgtypes.Grade, Username: "u", Subject: "Matematika", Fields: []string{"1.9.", "5"}}
			ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "Fizika"}
			ch <- msgtypes.Message{Code: msgtypes.Reading, Username: "u", Subject: "Lektira"}
			close(ch)

			err := tt.run(t.Context(), eDB, ch)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("entry point returned %v, want %v", err, tt.wantErr)
			}

			got := queue.FetchFailedMsgs(t.Context(), eDB, tt.queue)
			if len(got) != 3 {
				t.Fatalf("queued %d messages, want all 3 — dedup-flagged events cannot be re-scraped, so a misconfigured messenger must queue them", len(got))
			}

			subjects := make(map[string]bool, len(got))
			for _, q := range got {
				subjects[q.Msg.Subject] = true
			}

			for _, want := range []string{"Matematika", "Fizika", "Lektira"} {
				if !subjects[want] {
					t.Errorf("message %q was dropped rather than queued", want)
				}
			}
		})
	}
}

// TestMessengerMisconfigurationDuringShutdownStillQueues combines the two
// failure modes that overlap in production: a misconfigured messenger and an
// already-cancelled context. queueUndelivered uses queueStoreCtx
// (context.WithoutCancel), so the write must still land — otherwise a SIGTERM
// arriving in the same cycle as a config error loses every in-flight alert.
//
// Not parallel: the entry points touch package-level client globals.
func TestMessengerMisconfigurationDuringShutdownStillQueues(t *testing.T) {
	eDB := newEntryTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ch := make(chan msgtypes.Message, 1)
	ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "Kemija"}
	close(ch)

	if err := Slack(ctx, eDB, ch, SlackConfig{Retries: 1}); !errors.Is(err, ErrSlackEmptyAPIKey) {
		t.Fatalf("Slack() = %v, want ErrSlackEmptyAPIKey", err)
	}

	got := queue.FetchFailedMsgs(context.Background(), eDB, SlackQueueName)
	if len(got) != 1 || got[0].Msg.Subject != "Kemija" {
		t.Fatalf("FetchFailedMsgs = %+v, want the message persisted despite a cancelled context", got)
	}
}

// TestQueuedMessagesCarryQueuedAt checks the queue-age clock starts on the first
// failure. A zero QueuedAt would leave the row immune to MaxQueueAge and let it
// be retried forever.
func TestQueuedMessagesCarryQueuedAt(t *testing.T) {
	t.Parallel()

	eDB := newEntryTestDB(t)

	ch := make(chan msgtypes.Message, 1)
	ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "Biologija"}
	close(ch)

	queueUndelivered(t.Context(), eDB, []byte("queued-at-test"), ch)

	got := queue.FetchFailedMsgs(t.Context(), eDB, []byte("queued-at-test"))
	if len(got) != 1 {
		t.Fatalf("queued %d messages, want 1", len(got))
	}

	if got[0].Msg.QueuedAt.IsZero() {
		t.Error("QueuedAt is zero; MaxQueueAge would never expire this row")
	}
}

// TestQueueUndeliveredEmptyChannel: a closed, empty channel must be a clean
// no-op rather than writing a phantom row.
func TestQueueUndeliveredEmptyChannel(t *testing.T) {
	t.Parallel()

	eDB := newEntryTestDB(t)

	ch := make(chan msgtypes.Message)
	close(ch)

	queueUndelivered(t.Context(), eDB, []byte("empty-test"), ch)

	if got := queue.FetchFailedMsgs(t.Context(), eDB, []byte("empty-test")); len(got) != 0 {
		t.Errorf("FetchFailedMsgs = %+v, want no rows", got)
	}
}
