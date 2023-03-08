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
	"strconv"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	telegramSendDelay = 50 * time.Millisecond
)

var (
	ErrTelegramEmptyAPIKey    = errors.New("empty Telegram API key")
	ErrTelegramEmptyUserIds   = errors.New("empty list of Telegram Chat IDs")
	ErrTelegramInvalidChatID  = errors.New("invalid Telegram Chat ID")
	ErrTelegramSendingMessage = errors.New("error sending Telegram message")
)

// Telegram messenger processes events from a channel and attempts to communicate to one or more ChatIDs, optionally
// returning an error.
func Telegram(ctx context.Context, ch <-chan interface{}, apiKey string, chatIDs []string, retries uint) error {
	if apiKey == "" {
		return fmt.Errorf("%w", ErrTelegramEmptyAPIKey)
	}

	if len(chatIDs) == 0 {
		return fmt.Errorf("%w", ErrTelegramEmptyUserIds)
	}

	// new Telegram client
	bot, err := tgbotapi.NewBotAPI(apiKey)
	if err != nil {
		logger.Error().Msgf("Error creating Telegram session: %v", err)

		return err
	}

	logger.Debug().Msg("Sending message through Telegram")

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

			// format message as HTML
			m := format.HTMLMsg(g.Username, g.Subject, g.IsExam, g.Descriptions, g.Fields)

			// send to all recipients
			for _, u := range chatIDs {
				uu, err := strconv.ParseInt(u, 10, 64)
				if err != nil {
					logger.Error().Msgf("%v: %v", ErrTelegramInvalidChatID, err)

					return err
				}

				msg := tgbotapi.MessageConfig{
					BaseChat: tgbotapi.BaseChat{
						ChatID: uu,
					},
					Text:      m,
					ParseMode: tgbotapi.ModeHTML,
				}

				// retryable and cancellable attempt to send a message
				err = retry.Do(
					func() error {
						_, err := bot.Send(msg)

						return err
					},
					retry.Attempts(retries),
					retry.Context(ctx),
				)
				if err != nil {
					logger.Error().Msgf("%v: %v", ErrTelegramSendingMessage, err)

					break
				}

				time.Sleep(telegramSendDelay)
			}
		}
	}

	return err
}
