// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/format"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
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
	slackCli       *slack.Client
	slackMu        sync.Mutex // guards slackCli initialisation
	SlackVersion   = version.ReadVersion("github.com/slack-go/slack")
)

// SlackConfig holds the per-messenger settings for the Slack backend.
type SlackConfig struct {
	Token   string
	ChatIDs []string
	Retries uint
}

// Slack resends any queued failures, then delivers live messages from ch to the
// configured chat IDs. On init failure it drains ch into the queue so
// already-dedup-flagged events are not lost.
func Slack(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg SlackConfig) (err error) {
	// A send-path panic must drain ch and degrade, not crash the process.
	defer func() {
		if r := recover(); r != nil {
			err = recoverMessenger(ctx, eDB, SlackQueueName, ch, r)
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
		// Events are already dedup-flagged; queue them or they are lost forever.
		queueUndelivered(ctx, eDB, SlackQueueName, ch)

		return err
	}

	logger.Debug().Msgf("Started Slack messenger (%v)", SlackVersion)

	rl := ratelimit.New(SlackAPILimit, ratelimit.Per(SlackWindow))

	// Resend queued failures first. Rows are only removed after processing, so
	// a crash mid-loop re-delivers instead of losing; on shutdown, unprocessed
	// rows simply stay queued for the next run.
	for _, q := range queue.FetchFailedMsgs(ctx, eDB, SlackQueueName) {
		if ctx.Err() != nil {
			break
		}

		processSlack(ctx, eDB, q.Msg, cfg.ChatIDs, rl, cfg.Retries)

		// Failures were re-queued by processSlack; drop the original row.
		queue.Dequeue(ctx, eDB, q.Key)
	}

	// Drain fully; processSlack durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		processSlack(ctx, eDB, g, cfg.ChatIDs, rl, cfg.Retries)
	}

	return nil
}

// markSlackPermanent marks permanent errors as unrecoverable so retry-go stops
// retrying: 4xx StatusCodeError (except 408/429) and any SlackErrorResponse
// (API-level "ok":false, e.g. invalid_auth). Network/5xx errors stay transient.
func markSlackPermanent(err error) error {
	if err == nil {
		return nil
	}

	if sce, ok := errors.AsType[slack.StatusCodeError](err); ok {
		if isPermanentHTTPStatus(sce.Code) {
			return retry.Unrecoverable(err)
		}
	}

	if _, ok := errors.AsType[*slack.SlackErrorResponse](err); ok {
		return retry.Unrecoverable(err)
	}

	return err
}

// processSlack renders g as markup and sends it to each chat ID (Slack
// channel/user/group IDs, not @nicknames), re-queueing on partial or total
// failure. Recipients already in SkipRecipients are omitted.
func processSlack(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, chatIDs []string, rl ratelimit.Limiter, retries uint) {
	// Truncate over Slack's text cap — oversize bodies are rejected outright.
	m := truncateWithEllipsis(format.MarkupMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields), SlackMaxMessageChars)

	skipSet := make(map[string]struct{}, len(g.SkipRecipients))
	for _, r := range g.SkipRecipients {
		skipSet[r] = struct{}{}
	}

	var successfulIDs []string

	// Permanently-failed recipients: skipped on retry, never requeued.
	var poisonedIDs []string

	anyFailed := false
	// Tracks incomplete loops so shutdown-cancelled sends get re-queued, not dropped.
	allProcessed := true

	for _, u := range chatIDs {
		if _, skip := skipSet[u]; skip {
			continue
		}

		// Check before rl.Take() so shutdown is not blocked on a token.
		if ctx.Err() != nil {
			allProcessed = false

			break
		}

		rl.Take()

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
				// Permanent (channel_not_found, not_in_channel): drop, don't requeue.
				logger.Error().Msgf("%v: permanently dropping recipient %q: %v", ErrSlackSendingMessage, u, err)

				poisonedIDs = append(poisonedIDs, u)

				continue
			}

			logger.Error().Msgf("%v: %v", ErrSlackSendingMessage, err)

			anyFailed = true

			continue
		}

		successfulIDs = append(successfulIDs, u)
	}

	if anyFailed || !allProcessed {
		// Skip successful and poisoned recipients on retry; dedup bounds growth.
		g.SkipRecipients = mergeSkipRecipients(g.SkipRecipients, append(successfulIDs, poisonedIDs...))

		// Shutdown-tolerant: queue write must survive ctx cancel.
		sctx, scancel := queueStoreCtx(ctx)
		if err := queue.StoreFailedMsgs(sctx, eDB, SlackQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()
	}
}

// slackInit lazily creates the shared Slack client (idempotent).
func slackInit(token string) error {
	slackMu.Lock()
	defer slackMu.Unlock()

	if slackCli == nil {
		logger.Debug().Msg("Initializing Slack client")

		slackCli = slack.New(token)
	}

	return nil
}
