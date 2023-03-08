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
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/slack-go/slack"
)

const (
	slackSendDelay = 1000 * time.Millisecond
)

var (
	ErrSlackEmptyAPIKey    = errors.New("empty Slack API key")
	ErrSlackEmptyUserIds   = errors.New("empty list of Slack Chat IDs")
	ErrSlackSendingMessage = errors.New("error sending Slack message")
)

// Slack messenger processes events from a channel and attempts to communicate to one or more ChatIDs, optionally
// returning an error.
func Slack(ctx context.Context, ch <-chan interface{}, token string, chatIDs []string, retries uint) error {
	if token == "" {
		return fmt.Errorf("%w", ErrSlackEmptyAPIKey)
	}

	if len(chatIDs) == 0 {
		return fmt.Errorf("%w", ErrSlackEmptyUserIds)
	}

	// new full Slack client
	api := slack.New(token)

	logger.Debug().Msg("Sending message through Slack")

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
				u := u

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
				)
				if err != nil {
					logger.Error().Msgf("%v: %v", ErrSlackSendingMessage, err)

					break
				}

				time.Sleep(slackSendDelay)
			}
		}
	}

	return err
}
