// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/slack-go/slack"
	"go.uber.org/ratelimit"
)

// TestProcessShutdownRequeuesEveryMessenger: a recipient loop cut short by
// shutdown must still requeue the message. With the context already cancelled
// nothing is sent and nothing fails, so allProcessed is the only thing that can
// trigger the write — and every event reaching processX is already dedup-flagged,
// so dropping it here drops it for good.
//
// No client is configured on purpose: each loop must break before touching one.
// Not parallel: the messengers read package-level client globals.
func TestProcessShutdownRequeuesEveryMessenger(t *testing.T) {
	g := msgtypes.Message{
		Username:     "u",
		Subject:      "shutdown-mid-send",
		Descriptions: []string{"D"},
		Fields:       []string{"A"},
	}

	tests := []struct {
		name  string
		queue []byte
		run   func(ctx context.Context, eDB *sqlitedb.Edb, rl ratelimit.Limiter)
	}{
		{
			name:  "discord",
			queue: DiscordQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, rl ratelimit.Limiter) {
				processDiscord(ctx, eDB, g, []string{"user1", "user2"}, rl, 1)
			},
		},
		{
			name:  "telegram",
			queue: TelegramQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, rl ratelimit.Limiter) {
				processTelegram(ctx, eDB, g, []string{"123456789", "987654321"}, rl, 1)
			},
		},
		{
			name:  "slack",
			queue: SlackQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, rl ratelimit.Limiter) {
				processSlack(ctx, eDB, g, []string{"C12345678", "C87654321"}, rl, 1)
			},
		},
		{
			name:  "mail",
			queue: MailQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, rl ratelimit.Limiter) {
				processMail(ctx, eDB, g, []string{"a@example.com", "b@example.com"}, "from@example.com", "subject", rl, 1)
			},
		},
		{
			name:  "whatsapp",
			queue: WhatsAppQueueName,
			run: func(ctx context.Context, eDB *sqlitedb.Edb, rl ratelimit.Limiter) {
				processWhatsApp(ctx, nil, eDB, g, []string{"111@s.whatsapp.net", "222@s.whatsapp.net"}, rl, 1)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eDB, err := sqlitedb.New(context.Background(), filepath.Join(t.TempDir(), "requeue.db.sqlite"))
			if err != nil {
				t.Fatalf("sqlitedb.New() failed: %v", err)
			}

			defer eDB.Close() //nolint:errcheck

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			tt.run(ctx, eDB, ratelimit.New(1000))

			got := queue.FetchFailedMsgs(context.Background(), eDB, tt.queue)
			if len(got) != 1 || got[0].Msg.Subject != "shutdown-mid-send" {
				t.Fatalf("FetchFailedMsgs = %+v, want the message requeued after a shutdown-interrupted recipient loop; an unreached recipient is lost forever otherwise",
					got)
			}
		})
	}
}

// TestPoisonedRecipientsRecordedInSkipRecipients: a permanently-failed recipient
// must be recorded in SkipRecipients, or every retry re-attempts it once per
// cycle until MaxQueueAge, against APIs that rate-limit and ban for abuse.
//
// Each case pairs a recipient invalid on its face (poisons with no network) with
// a cancelled context (forces the requeue). Slack and WhatsApp are covered
// separately: their poison paths need a configured client, and WhatsApp checks
// the context before parsing the JID, so nothing can poison first.
// Not parallel: the messengers read package-level client globals.
func TestPoisonedRecipientsRecordedInSkipRecipients(t *testing.T) {
	g := msgtypes.Message{
		Username:     "u",
		Subject:      "poisoned-plus-shutdown",
		Descriptions: []string{"D"},
		Fields:       []string{"A"},
	}

	tests := []struct {
		name        string
		queue       []byte
		wantSkipped string
		run         func(ctx context.Context, eDB *sqlitedb.Edb, rl ratelimit.Limiter)
	}{
		{
			name:        "telegram malformed chat ID",
			queue:       TelegramQueueName,
			wantSkipped: "not-a-chat-id",
			run: func(ctx context.Context, eDB *sqlitedb.Edb, rl ratelimit.Limiter) {
				// First ID fails ParseInt (poison, no network); the second
				// reaches the context check and breaks the loop.
				processTelegram(ctx, eDB, g, []string{"not-a-chat-id", "123456789"}, rl, 1)
			},
		},
		{
			name:        "mail malformed recipient",
			queue:       MailQueueName,
			wantSkipped: "not-an-email",
			run: func(ctx context.Context, eDB *sqlitedb.Edb, rl ratelimit.Limiter) {
				// The invalid address poisons while the batch is built; the
				// cancelled context then stops the batch from being sent.
				processMail(ctx, eDB, g, []string{"not-an-email", "ok@example.com"}, "from@example.com", "subject", rl, 1)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eDB, err := sqlitedb.New(context.Background(), filepath.Join(t.TempDir(), "poisoned.db.sqlite"))
			if err != nil {
				t.Fatalf("sqlitedb.New() failed: %v", err)
			}

			defer eDB.Close() //nolint:errcheck

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			tt.run(ctx, eDB, ratelimit.New(1000))

			got := queue.FetchFailedMsgs(context.Background(), eDB, tt.queue)
			if len(got) != 1 {
				t.Fatalf("FetchFailedMsgs = %+v, want the message requeued once", got)
			}

			if !slices.Contains(got[0].Msg.SkipRecipients, tt.wantSkipped) {
				t.Errorf("SkipRecipients = %v, want it to contain the poisoned recipient %q; otherwise it is retried every cycle until MaxQueueAge",
					got[0].Msg.SkipRecipients, tt.wantSkipped)
			}
		})
	}
}

// stubSlackPoster returns a scripted error per channel ID.
type stubSlackPoster struct {
	errs map[string]error
}

func (s *stubSlackPoster) PostMessageContext(_ context.Context, channelID string,
	_ ...slack.MsgOption,
) (string, string, error) {
	return channelID, "", s.errs[channelID]
}

// TestSlackPoisonedRecipientRecordedInSkipRecipients: a SlackErrorResponse is an
// API-level rejection that never succeeds on retry, so the channel is dropped —
// but it must still reach SkipRecipients, or the requeued message re-attempts it
// every cycle until MaxQueueAge. The transient second channel forces the requeue.
// Not parallel: writes the package-level slackCli global.
func TestSlackPoisonedRecipientRecordedInSkipRecipients(t *testing.T) {
	const (
		dead  = "C0000DEAD"
		flaky = "C0000FLAK"
	)

	deadErr := &slack.SlackErrorResponse{Err: "channel_not_found"}

	slackMu.Lock()
	orig := slackCli
	slackCli = &stubSlackPoster{errs: map[string]error{
		dead:  deadErr,
		flaky: errors.New("connection reset by peer"),
	}}
	slackMu.Unlock()

	t.Cleanup(func() {
		slackMu.Lock()
		slackCli = orig
		slackMu.Unlock()
	})

	eDB, err := sqlitedb.New(context.Background(), filepath.Join(t.TempDir(), "slack-poison.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	g := msgtypes.Message{
		Username:     "u",
		Subject:      "slack-poison",
		Descriptions: []string{"D"},
		Fields:       []string{"A"},
	}

	processSlack(context.Background(), eDB, g, []string{dead, flaky}, ratelimit.New(1000), 1)

	failed := queue.FetchFailedMsgs(context.Background(), eDB, SlackQueueName)
	if len(failed) != 1 {
		t.Fatalf("FetchFailedMsgs = %+v, want the message requeued for the transient failure", failed)
	}

	if !slices.Contains(failed[0].Msg.SkipRecipients, dead) {
		t.Errorf("SkipRecipients = %v, want the permanently-failed channel recorded; otherwise it is re-attempted every cycle until MaxQueueAge",
			failed[0].Msg.SkipRecipients)
	}
}
