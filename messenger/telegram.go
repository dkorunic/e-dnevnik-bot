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
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/queue"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/version"
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
	telegramMu        sync.Mutex // guards telegramCli initialisation
	TelegramVersion   = version.ReadVersion("github.com/go-telegram/bot")
)

// Telegram sends messages through the Telegram API to the specified Telegram chat IDs.
//
// It takes the following parameters:
// - ctx: the context.Context object for managing the execution of the function.
// - eDB: the database instance for checking and storing failed messages.
// - ch: the channel from which to receive messages.
// - apiKey: the Telegram API key.
// - chatIDs: a slice of strings containing the Telegram chat IDs to send the message to.
// - retries: the number of retry attempts for sending the message.
//
// The function formats the message as HTML and attempts to send it to each chat ID.
// It logs errors for invalid chat IDs, sending failures, and stores failed messages for retry.
// It uses rate limiting and supports retries with delay.
func Telegram(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, apiKey string, chatIDs []string, retries uint) error {
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

	logger.Debug().Msgf("Started Telegram messenger (%v)", TelegramVersion)

	rl := ratelimit.New(TelegramAPILimit, ratelimit.Per(TelegramWindow))

	// process all failed messages; re-queue any unprocessed on cancellation
	failedMsgs := queue.FetchFailedMsgs(ctx, eDB, TelegramQueueName)
	for i, g := range failedMsgs {
		if ctx.Err() != nil {
			queue.RequeueMsgs(eDB, TelegramQueueName, failedMsgs[i:])

			return ctx.Err()
		}

		processTelegram(ctx, eDB, g, chatIDs, rl, retries)

		if ctx.Err() != nil {
			queue.RequeueMsgs(eDB, TelegramQueueName, failedMsgs[i+1:])

			return ctx.Err()
		}
	}

	// process all messages
	for g := range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processTelegram(ctx, eDB, g, chatIDs, rl, retries)
		}
	}

	return nil
}

// processTelegram processes a message and sends it to the specified Telegram chat IDs.
//
// It takes the following parameters:
// - ctx: the context.Context object for managing the execution of the function.
// - eDB: the database instance for checking and storing failed messages.
// - g: the message to be processed and sent.
// - chatIDs: a slice of strings containing the Telegram chat IDs to send the message to.
// - rl: the rate limiter to control the message sending rate.
// - retries: the number of retry attempts for sending the message.
//
// The function formats the message as HTML and attempts to send it to each chat ID.
// It logs errors for invalid chat IDs, sending failures, and stores failed messages for retry.
// It uses rate limiting and supports retries with delay.
func processTelegram(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, chatIDs []string, rl ratelimit.Limiter, retries uint) {
	// format message as HTML; truncate to Telegram's sendMessage limit so an
	// over-long body is delivered (slightly lossy) rather than hard-rejected.
	m := truncateWithEllipsis(format.HTMLMsg(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields), TelegramMaxMessageChars)

	// build a skip set for O(1) lookups
	skipSet := make(map[string]struct{}, len(g.SkipRecipients))
	for _, r := range g.SkipRecipients {
		skipSet[r] = struct{}{}
	}

	var successfulIDs []string

	anyFailed := false
	// allProcessed is set to false when the recipient loop breaks due to
	// cancellation before finishing; it forces the message back into the
	// queue so a shutdown-cancelled send is not silently dropped.
	allProcessed := true

	// send to all recipients
	for _, u := range chatIDs {
		// Skip recipients that already received this message on a previous attempt.
		if _, skip := skipSet[u]; skip {
			continue
		}

		uu, err := strconv.ParseInt(u, 10, 64)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrTelegramInvalidChatID, err)

			continue
		}

		msg := bot.SendMessageParams{
			ChatID:    uu,
			Text:      m,
			ParseMode: models.ParseModeHTML,
		}

		// Honour cancellation before blocking on the rate limiter so shutdown
		// is not delayed by a pending token.
		if ctx.Err() != nil {
			allProcessed = false

			break
		}

		rl.Take()

		// retryable and cancellable attempt to send a message
		err = retry.New(
			retry.Attempts(retries),
			retry.Context(ctx),
			retry.Delay(TelegramMinDelay),
		).Do(
			func() error {
				_, err := telegramCli.SendMessage(ctx, &msg)

				return err
			},
		)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrTelegramSendingMessage, err)

			anyFailed = true

			continue
		}

		successfulIDs = append(successfulIDs, u)
	}

	if anyFailed || !allProcessed {
		// Record who succeeded so they are skipped on the next retry. Merge
		// with dedup: repeated partial failures against the same recipient
		// would otherwise append duplicate entries on every cycle.
		g.SkipRecipients = mergeSkipRecipients(g.SkipRecipients, successfulIDs)

		sctx, scancel := queueStoreCtx(ctx)
		if err := queue.StoreFailedMsgs(sctx, eDB, TelegramQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()
	}
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
	telegramMu.Lock()
	defer telegramMu.Unlock()

	var err error

	if telegramCli == nil {
		logger.Debug().Msg("Initializing Telegram client")

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
