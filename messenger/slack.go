// @license
// Copyright (C) 2022  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

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

	// process all failed messages; re-queue any unprocessed on cancellation
	failedMsgs := queue.FetchFailedMsgs(ctx, eDB, SlackQueueName)
	for i, g := range failedMsgs {
		if ctx.Err() != nil {
			queue.RequeueMsgs(eDB, SlackQueueName, failedMsgs[i:])

			return ctx.Err()
		}

		processSlack(ctx, eDB, g, chatIDs, rl, retries)

		if ctx.Err() != nil {
			queue.RequeueMsgs(eDB, SlackQueueName, failedMsgs[i+1:])

			return ctx.Err()
		}
	}

	// process all messages
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
	// format message as Markup
	m := format.MarkupMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields)

	// build a skip set for O(1) lookups
	skipSet := make(map[string]struct{}, len(g.SkipRecipients))
	for _, r := range g.SkipRecipients {
		skipSet[r] = struct{}{}
	}

	var successfulIDs []string

	anyFailed := false

	// send to all recipients: channels and nicknames are permitted
	for _, u := range chatIDs {
		// Skip recipients that already received this message on a previous attempt.
		if _, skip := skipSet[u]; skip {
			continue
		}

		// Honour cancellation before blocking on the rate limiter so shutdown
		// is not delayed by a pending token.
		if ctx.Err() != nil {
			break
		}

		rl.Take()

		// retryable and cancellable attempt to send a message
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

				return err
			},
		)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrSlackSendingMessage, err)

			anyFailed = true

			continue
		}

		successfulIDs = append(successfulIDs, u)
	}

	if anyFailed {
		// Record who succeeded so they are skipped on the next retry.
		g.SkipRecipients = append(g.SkipRecipients, successfulIDs...)

		if err := queue.StoreFailedMsgs(ctx, eDB, SlackQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}
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
