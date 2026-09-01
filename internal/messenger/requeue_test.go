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
	"go.uber.org/ratelimit"
)

// TestProcessShutdownRequeuesEveryMessenger pins the shutdown half of the
// requeue condition across every backend that fans out to recipients.
//
// Each processX loop checks ctx.Err() before spending rate-limit budget and
// records allProcessed=false when it breaks early. The requeue must fire on
// that flag as well as on anyFailed: with the context already cancelled
// nothing is sent and nothing fails, so anyFailed stays false and allProcessed
// is the only thing left to trigger the write. Narrowing the condition to
// `if anyFailed` therefore loses the message outright — and because every event
// reaching processX has already been dedup-flagged, the portal will never
// re-offer it. A SIGTERM landing mid-send becomes permanent data loss.
//
// No network client is configured on purpose: each loop must break before it
// touches one, so a nil/unconfigured client is proof the send path was never
// entered.
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

// TestPoisonedRecipientsRecordedInSkipRecipients covers the second half of the
// SkipRecipients merge: permanently-failed recipients must be recorded there
// alongside the successful ones.
//
// A poisoned recipient is dropped rather than requeued, but the *message* is
// still requeued whenever anything else failed or the loop was cut short. If
// the poisoned ID is not in SkipRecipients, every retry re-attempts a recipient
// that can never accept the message — once per message per cycle for the full
// 30 days of MaxQueueAge, against APIs that rate-limit and ban for abuse.
//
// Both cases pair a recipient that is invalid on its face (so it poisons with
// no network involved) with a cancelled context (so the loop breaks early and
// the message is requeued). That combination reaches the merge deterministically
// without a live client.
//
// Slack and WhatsApp are absent deliberately: neither has a poison path that can
// be reached without a configured client — Slack poisons only on an API error
// response, and WhatsApp checks the context before parsing the JID, so a
// cancelled context breaks before any recipient can poison.
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
