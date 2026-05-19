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
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/queue"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/version"
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

// Slack sends messages through the Slack API.
//
// ctx: the context in which the function is executed.
// eDB: the database instance for checking failed messages.
// ch: the channel from which messages are received.
// token: the Slack API key.
// chatIDs: the IDs of the recipients.
// retries: the number of retries in case of failure.
// error: an error if there was a problem sending the message.
func Slack(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, token string, chatIDs []string, retries uint) error {
	if token == "" {
		return fmt.Errorf("%w", ErrSlackEmptyAPIKey)
	}

	if len(chatIDs) == 0 {
		return fmt.Errorf("%w", ErrSlackEmptyUserIDs)
	}

	if err := slackInit(token); err != nil {
		return err
	}

	logger.Debug().Msgf("Started Slack messenger (%v)", SlackVersion)

	rl := ratelimit.New(SlackAPILimit, ratelimit.Per(SlackWindow))

	// Drain queued failures first; re-queue tail on shutdown.
	failedMsgs := queue.FetchFailedMsgs(ctx, eDB, SlackQueueName)
	for i, g := range failedMsgs {
		if ctx.Err() != nil {
			queue.RequeueMsgs(ctx, eDB, SlackQueueName, failedMsgs[i:])

			return ctx.Err()
		}

		processSlack(ctx, eDB, g, chatIDs, rl, retries)

		if ctx.Err() != nil {
			queue.RequeueMsgs(ctx, eDB, SlackQueueName, failedMsgs[i+1:])

			return ctx.Err()
		}
	}

	for g := range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processSlack(ctx, eDB, g, chatIDs, rl, retries)
		}
	}

	return nil
}

// markSlackPermanent wraps Slack API errors that will never succeed on retry
// in retry.Unrecoverable so retry-go short-circuits the remaining attempts.
//
// Two categories qualify as permanent:
//   - slack.StatusCodeError with a 4xx code (auth failure, channel missing,
//     malformed request) — except 408 (timeout) and 429 (rate-limit), which
//     are transient.
//   - slack.SlackErrorResponse — API-level "ok":false responses carry an
//     Err field naming a specific API error (e.g. "invalid_auth",
//     "channel_not_found"); none of these clear up on retry.
//
// Everything else (network errors, 5xx, unclassified) falls through unchanged
// and keeps its normal retry budget.
func markSlackPermanent(err error) error {
	if err == nil {
		return nil
	}

	if sce, ok := errors.AsType[slack.StatusCodeError](err); ok {
		if sce.Code >= 400 && sce.Code < 500 && sce.Code != 408 && sce.Code != 429 {
			return retry.Unrecoverable(err)
		}
	}

	if ser, ok := errors.AsType[*slack.SlackErrorResponse](err); ok {
		return retry.Unrecoverable(err)
	}

	return err
}

// processSlack sends a message to a list of Slack chat IDs.
//
// It takes the following parameters:
// - ctx: the context.Context object for managing the execution of the function.
// - eDB: the database instance for checking failed messages.
// - g: the message to be processed and sent.
// - chatIDs: a slice of strings containing the IDs of the recipients (Slack channels).
// - rl: the rate limiter to control the message sending rate.
// - err: an error that occurred during message sending.
// - retries: the number of retry attempts for sending the message.
//
// The function formats the message as Markup and attempts to send it to each chat ID.
// It logs errors for sending failures, and stores failed messages for retry.
// It uses rate limiting and supports retries with delay.
func processSlack(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, chatIDs []string, rl ratelimit.Limiter, retries uint) {
	// Truncate over Slack's text cap — oversize bodies are rejected outright.
	m := truncateWithEllipsis(format.MarkupMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields), SlackMaxMessageChars)

	skipSet := make(map[string]struct{}, len(g.SkipRecipients))
	for _, r := range g.SkipRecipients {
		skipSet[r] = struct{}{}
	}

	var successfulIDs []string

	anyFailed := false
	// Tracks incomplete loops so shutdown-cancelled sends get re-queued, not dropped.
	allProcessed := true

	// chatIDs may be channels or nicknames; Slack resolves either.
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
			logger.Error().Msgf("%v: %v", ErrSlackSendingMessage, err)

			anyFailed = true

			continue
		}

		successfulIDs = append(successfulIDs, u)
	}

	if anyFailed || !allProcessed {
		// Dedup prevents unbounded SkipRecipients growth across retries.
		g.SkipRecipients = mergeSkipRecipients(g.SkipRecipients, successfulIDs)

		// Shutdown-tolerant: queue write must survive ctx cancel.
		sctx, scancel := queueStoreCtx(ctx)
		if err := queue.StoreFailedMsgs(sctx, eDB, SlackQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()
	}
}

// slackInit initializes the Slack client using the provided API token.
//
// token: The Slack API key used to authenticate and create a new Slack client.
// If the Slack client is not already initialized, the function creates a new
// Slack client. If the client has already been initialized, the function does
// nothing.
func slackInit(token string) error {
	slackMu.Lock()
	defer slackMu.Unlock()

	if slackCli == nil {
		logger.Debug().Msg("Initializing Slack client")

		slackCli = slack.New(token)
	}

	return nil
}
