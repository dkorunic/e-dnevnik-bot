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
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/slack-go/slack"
	"go.uber.org/ratelimit"
)

const (
	SlackAPILImit = 20 // typically 20 req/min per user
	SlackWindow   = 1 * time.Minute
	SlackMinDelay = SlackWindow / SlackAPILImit
	SlackQueue    = "slack-queue"
)

var (
	ErrSlackEmptyAPIKey    = errors.New("empty Slack API key")
	ErrSlackEmptyUserIDs   = errors.New("empty list of Slack Chat IDs")
	ErrSlackSendingMessage = errors.New("error sending Slack message")

	SlackQueueName = []byte(SlackQueue)
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
func Slack(ctx context.Context, eDB *db.Edb, ch <-chan interface{}, token string, chatIDs []string, retries uint) error {
	if token == "" {
		return fmt.Errorf("%w", ErrSlackEmptyAPIKey)
	}

	if len(chatIDs) == 0 {
		return fmt.Errorf("%w", ErrSlackEmptyUserIDs)
	}

	// new full Slack client
	api := slack.New(token)

	logger.Debug().Msg("Started Slack messenger")

	rl := ratelimit.New(SlackAPILImit, ratelimit.Per(SlackWindow))

	var err error

	// process all messages
	for o := range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			g, ok := o.(msgtypes.Message)
			if !ok {
				logger.Warn().Msg("Received invalid type from channel, trying to continue")

				continue
			}

			// format message as Markup
			m := format.MarkupMsg(g.Username, g.Subject, g.IsExam, g.Descriptions, g.Fields)

			// send to all recipients: channels and nicknames are permitted
			for _, u := range chatIDs {
				rl.Take()

				// retryable and cancellable attempt to send a message
				err = retry.Do(
					func() error {
						_, _, err := api.PostMessage(u,
							slack.MsgOptionText(m, false),
							slack.MsgOptionAsUser(true),
						)

						return err
					},
					retry.Attempts(retries),
					retry.Context(ctx),
					retry.Delay(SlackMinDelay),
				)
				if err != nil {
					logger.Error().Msgf("%v: %v", ErrSlackSendingMessage, err)

					// store failed message
					if err := storeFailedMsgs(eDB, SlackQueueName, g); err != nil {
						logger.Error().Msgf("%v: %v", ErrQueueing, err)
					}

					continue
				}
			}
		}
	}

	return nil
}
