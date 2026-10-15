// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/format"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/internal/version"
	"github.com/slack-go/slack"
	"go.uber.org/ratelimit"
)

const (
	SlackAPILimit = 20 // typically 20 req/min per user
	SlackWindow   = 1 * time.Minute
	SlackMinDelay = SlackWindow / SlackAPILimit
	SlackQueue    = "slack-queue"
)

var (
	ErrSlackEmptyAPIKey    = errors.New("empty Slack API key")
	ErrSlackEmptyUserIDs   = errors.New("empty list of Slack Chat IDs")
	ErrSlackSendingMessage = errors.New("error sending Slack message")

	SlackQueueName = []byte(SlackQueue)
	slackCli       slackPoster
	slackCreds     credGuard  // credentials slackCli was built from
	slackMu        sync.Mutex // guards slackCli and slackCreds
	SlackVersion   = version.ReadVersion("github.com/slack-go/slack")
)

// slackPoster is the slice of *slack.Client the send path uses, so tests can
// drive permanent-vs-transient outcomes without a workspace.
type slackPoster interface {
	PostMessageContext(ctx context.Context, channelID string, options ...slack.MsgOption) (string, string, error)
}

// SlackConfig holds the per-messenger settings for the Slack backend.
type SlackConfig struct {
	Token   string
	ChatIDs []string
	Retries uint
}

// Slack resends queued failures, then delivers ch to the configured chat IDs.
// On init failure it drains ch to the queue rather than lose flagged events.
func Slack(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg SlackConfig) (err error) {
	// Panic guard; stays nil on the resend path (see recoverMessenger).
	var inflight *msgtypes.Message

	defer func() {
		if r := recover(); r != nil {
			err = recoverMessenger(ctx, eDB, SlackQueueName, ch, r, inflight)
		}
	}()

	if cfg.Token == "" {
		queueUndelivered(ctx, eDB, SlackQueueName, ch)

		return fmt.Errorf("%w", ErrSlackEmptyAPIKey)
	}

	if len(cfg.ChatIDs) == 0 {
		queueUndelivered(ctx, eDB, SlackQueueName, ch)

		return fmt.Errorf("%w", ErrSlackEmptyUserIDs)
	}

	if err := slackInit(cfg.Token); err != nil {
		// Already dedup-flagged: queue them or lose them forever.
		queueUndelivered(ctx, eDB, SlackQueueName, ch)

		return err
	}

	logger.Debug().Msgf("Started Slack messenger (%v)", SlackVersion)

	rl := ratelimit.New(SlackAPILimit, ratelimit.Per(SlackWindow))

	var cs cycleSender

	resendQueued(ctx, eDB, SlackQueueName, func(m msgtypes.Message) {
		cs.send(ctx, eDB, SlackQueueName, m, func() bool {
			return processSlack(ctx, eDB, m, cfg.ChatIDs, rl, cfg.Retries)
		})
	})

	// Drain fully; processSlack durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		cs.send(ctx, eDB, SlackQueueName, g, func() bool {
			return processSlack(ctx, eDB, g, cfg.ChatIDs, rl, cfg.Retries)
		})
		inflight = nil
	}

	return nil
}

// slackTransientErrors are API-level "ok":false codes Slack documents as
// server-side and worth retrying.
var slackTransientErrors = []string{"internal_error", "fatal_error", "service_unavailable", "request_timeout", "ratelimited"}

// slackChannelErrors condemn one channel or one message and nothing else, so
// that recipient is poison-dropped.
var slackChannelErrors = []string{
	"channel_not_found", "not_in_channel", "is_archived", "msg_too_long", "no_text",
	"invalid_blocks", "restricted_action", "cannot_reply_to_message",
}

// slackAuthErrors are token-wide: every channel fails alike, so they are config
// errors and stop the cycle (see cycleSender).
var slackAuthErrors = []string{
	"invalid_auth", "not_authed", "token_revoked", "token_expired", "account_inactive",
	"missing_scope", "not_allowed_token_type", "no_permission", "team_access_not_granted",
}

// markSlackPermanent classifies an error by how far it reaches:
//   - transient (network, 5xx, slackTransientErrors): retried;
//   - one channel (permanent 4xx StatusCodeError, slackChannelErrors):
//     poison-dropped;
//   - every channel (slackAuthErrors): a config error, queued, stops the cycle;
//   - anything else: retrying is pointless, but its reach is unknown, so that
//     recipient alone is requeued and the loop moves on. Treating an unknown
//     per-channel code as token-wide would block every channel for 30 days;
//     treating it as permanent would drop the alert.
//
// SlackErrorResponse is matched by value: SlackResponse.Err returns it that
// way, so a pointer match never fires and every API error read as transient.
func markSlackPermanent(err error) error {
	if err == nil {
		return nil
	}

	if sce, ok := errors.AsType[slack.StatusCodeError](err); ok {
		if isPermanentHTTPStatus(sce.Code) {
			// Inner sentinel survives retry.Do's marker stripping.
			return retry.Unrecoverable(permanentError{err})
		}
	}

	if ser, ok := errors.AsType[slack.SlackErrorResponse](err); ok {
		switch {
		case slices.Contains(slackTransientErrors, ser.Err):
			return err
		case slices.Contains(slackChannelErrors, ser.Err):
			// Inner sentinel survives retry.Do's marker stripping.
			return retry.Unrecoverable(permanentError{err})
		case slices.Contains(slackAuthErrors, ser.Err):
			return markConfigErr(err)
		default:
			// Neither marker: run.failed requeues this recipient only.
			return retry.Unrecoverable(err)
		}
	}

	return err
}

// slackMessageText renders g within Slack's text cap. Pairs are dropped before
// rendering rather than cut after: the fence and the &-entities Slack requires
// survive only an untouched string (see truncateRendered).
func slackMessageText(g msgtypes.Message) string {
	return truncateRendered(format.MarkupMsg, g.Username, g.Subject, g.Code, g.Descriptions, g.Fields, SlackMaxMessageChars)
}

func processSlack(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, chatIDs []string, rl ratelimit.Limiter, retries uint) bool {
	m := slackMessageText(g)

	run := newRecipientRun(g)

	cfgFailed := false

	for _, u := range chatIDs {
		if run.skipped(u) {
			continue
		}

		// Shutdown must not wait out a token: interrupt and requeue.
		if !takeCtx(ctx, rl) {
			run.interrupt()

			break
		}

		err := retry.New(
			retry.Attempts(retries),
			retry.Context(ctx),
			retry.Delay(SlackMinDelay),
		).Do(
			func() error {
				_, _, err := slackCli.PostMessageContext(ctx,
					u,
					slack.MsgOptionText(m, false),
					slack.MsgOptionAsUser(true),
				)

				return markSlackPermanent(err)
			},
		)
		if err != nil {
			if isPermanentSendErr(err) {
				// channel_not_found, not_in_channel: drop, don't requeue.
				logger.Error().Msgf("%v: permanently dropping recipient %q: %v", ErrSlackSendingMessage, u, err)

				run.poison(u)

				continue
			}

			logger.Error().Msgf("%v: %v", ErrSlackSendingMessage, err)

			run.failed()

			// Bad for every recipient: stop, and let the cycle queue the rest.
			if isConfigSendErr(err) {
				cfgFailed = true

				break
			}

			continue
		}

		run.delivered(u)
	}

	run.finish(ctx, eDB, SlackQueueName, g)

	return cfgFailed
}

// slackInit lazily builds the shared client, rebuilding on a token change.
func slackInit(token string) error {
	slackMu.Lock()
	defer slackMu.Unlock()

	if slackCli != nil && !slackCreds.changed(token) {
		return nil
	}

	logger.Debug().Msg("Initializing Slack client")

	slackCli = slack.New(token)
	slackCreds.record(token)

	return nil
}
