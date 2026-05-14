// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
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

	// Drain queued failures first; re-queue tail on shutdown.
	failedMsgs := queue.FetchFailedMsgs(ctx, eDB, TelegramQueueName)
	for i, g := range failedMsgs {
		if ctx.Err() != nil {
			queue.RequeueMsgs(ctx, eDB, TelegramQueueName, failedMsgs[i:])

			return ctx.Err()
		}

		processTelegram(ctx, eDB, g, chatIDs, rl, retries)

		if ctx.Err() != nil {
			queue.RequeueMsgs(ctx, eDB, TelegramQueueName, failedMsgs[i+1:])

			return ctx.Err()
		}
	}

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

// markTelegramPermanent wraps Telegram API errors that will never succeed on
// retry in retry.Unrecoverable so retry-go short-circuits the remaining
// attempts.
//
// Permanent categories:
//   - Forbidden (bot blocked), BadRequest (malformed), Unauthorized
//     (invalid token), NotFound (chat gone), Conflict.
//   - MigrateError: the chat was upgraded to a supergroup, the old ChatID will
//     never accept sends again.
//
// TooManyRequests (both the sentinel and *TooManyRequestsError) is transient
// and keeps its normal retry budget. Network/transport errors fall through
// unchanged.
func markTelegramPermanent(err error) error {
	if err == nil {
		return nil
	}

	// errors.As survives wrapping in future library versions.
	var tmr *bot.TooManyRequestsError
	if errors.As(err, &tmr) || errors.Is(err, bot.ErrorTooManyRequests) {
		return err
	}

	var mig *bot.MigrateError
	if errors.As(err, &mig) ||
		errors.Is(err, bot.ErrorForbidden) ||
		errors.Is(err, bot.ErrorBadRequest) ||
		errors.Is(err, bot.ErrorUnauthorized) ||
		errors.Is(err, bot.ErrorNotFound) ||
		errors.Is(err, bot.ErrorConflict) {
		return retry.Unrecoverable(err)
	}

	return err
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
	// Trim pairs pre-format to keep <b>/<pre> tags balanced.
	m := truncateHTMLBody(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields, TelegramMaxMessageChars)

	skipSet := make(map[string]struct{}, len(g.SkipRecipients))
	for _, r := range g.SkipRecipients {
		skipSet[r] = struct{}{}
	}

	var successfulIDs []string

	anyFailed := false
	// Tracks incomplete loops so shutdown-cancelled sends get re-queued, not dropped.
	allProcessed := true

	for _, u := range chatIDs {
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

		// Check before rl.Take() so shutdown is not blocked on a token.
		if ctx.Err() != nil {
			allProcessed = false

			break
		}

		rl.Take()

		err = retry.New(
			retry.Attempts(retries),
			retry.Context(ctx),
			retry.Delay(TelegramMinDelay),
		).Do(
			func() error {
				_, err := telegramCli.SendMessage(ctx, &msg)

				return markTelegramPermanent(err)
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
		// Dedup prevents unbounded SkipRecipients growth across retries.
		g.SkipRecipients = mergeSkipRecipients(g.SkipRecipients, successfulIDs)

		// Shutdown-tolerant: queue write must survive ctx cancel.
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

		telegramCli, err = bot.New(apiKey)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrTelegramSession, err)

			return err
		}

		// Start blocks until ctx cancel; run off the main goroutine.
		go telegramCli.Start(ctx)
	}

	return nil
}
