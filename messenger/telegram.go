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
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"go.uber.org/ratelimit"
)

const (
	TelegramAPILimit = 20 // 20 API req/min per user
	TelegramWindow   = 1 * time.Minute
	TelegramMinDelay = TelegramWindow / TelegramAPILimit
	TelegramQueue    = "telegram-queue"
)

var (
	ErrTelegramSession        = errors.New("error creating Telegram session")
	ErrTelegramEmptyAPIKey    = errors.New("empty Telegram API key")
	ErrTelegramEmptyUserIDs   = errors.New("empty list of Telegram Chat IDs")
	ErrTelegramInvalidChatID  = errors.New("invalid Telegram Chat ID")
	ErrTelegramSendingMessage = errors.New("error sending Telegram message")

	TelegramQueueName = []byte(TelegramQueue)
	telegramCli       *bot.Bot
)

// Telegram sends messages through the Telegram API.
//
// It takes the following parameters:
// - ctx: the context.Context object for handling deadlines and cancellations.
// - eDB: the database instance for checking failed messages.
// - ch: a channel for receiving messages to be sent.
// - apiKey: the API key for accessing the Telegram API.
// - chatIDs: a slice of strings containing the IDs of the chat recipients.
// - retries: the number of times to retry sending a message in case of failure.
//
// It returns an error indicating any failures that occurred during the process.
func Telegram(ctx context.Context, eDB *db.Edb, ch <-chan msgtypes.Message, apiKey string, chatIDs []string, retries uint) error {
	if apiKey == "" {
		return fmt.Errorf("%w", ErrTelegramEmptyAPIKey)
	}

	if len(chatIDs) == 0 {
		return fmt.Errorf("%w", ErrTelegramEmptyUserIDs)
	}

	err := telegramInit(ctx, apiKey)
	if err != nil {
		return err
	}

	logger.Debug().Msg("Started Telegram messenger")

	rl := ratelimit.New(TelegramAPILimit, ratelimit.Per(TelegramWindow))

	// process all messages
	for g := range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			// format message as HTML
			m := format.HTMLMsg(g.Username, g.Subject, g.IsExam, g.Descriptions, g.Fields)

			// send to all recipients
			for _, u := range chatIDs {
				uu, err := strconv.ParseInt(u, 10, 64)
				if err != nil {
					logger.Error().Msgf("%v: %v", ErrTelegramInvalidChatID, err)

					return err
				}

				msg := bot.SendMessageParams{
					ChatID:    uu,
					Text:      m,
					ParseMode: models.ParseModeHTML,
				}

				rl.Take()

				// retryable and cancellable attempt to send a message
				err = retry.Do(
					func() error {
						_, err := telegramCli.SendMessage(ctx, &msg)

						return err
					},
					retry.Attempts(retries),
					retry.Context(ctx),
					retry.Delay(TelegramMinDelay),
				)
				if err != nil {
					logger.Error().Msgf("%v: %v", ErrTelegramSendingMessage, err)

					// store failed message
					if err := storeFailedMsgs(eDB, TelegramQueueName, g); err != nil {
						logger.Error().Msgf("%v: %v", ErrQueueing, err)
					}

					continue
				}
			}
		}
	}

	return nil
}

// telegramInit initializes a Telegram client and starts a session if it has not been initialized yet.
//
// The function takes a context.Context and the Telegram API key as parameters.
// If the Telegram client has not been initialized yet (i.e., telegramCli is nil), it creates a new client using the provided API key,
// starts the session, and assigns the client to the global telegramCli variable.
// If the client has already been initialized, the function does nothing and returns nil.
//
// The function returns an error if there was a problem creating the client or starting the session.
func telegramInit(ctx context.Context, apiKey string) error {
	var err error

	if telegramCli == nil {
		// create a Telegram bot session
		telegramCli, err = bot.New(apiKey)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrTelegramSession, err)

			return err
		}

		// needs a separate goroutine
		go telegramCli.Start(ctx)
	}

	return nil
}
