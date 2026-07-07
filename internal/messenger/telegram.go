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
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/internal/version"
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

// TelegramConfig holds the per-messenger settings for the Telegram backend.
type TelegramConfig struct {
	Token   string
	ChatIDs []string
	Retries uint
}

// Telegram resends any queued failures, then delivers live messages from ch to
// the configured chat IDs. On init failure it drains ch into the queue so
// already-dedup-flagged events are not lost.
func Telegram(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg TelegramConfig) error {
	if cfg.Token == "" {
		queueUndelivered(ctx, eDB, TelegramQueueName, ch)

		return fmt.Errorf("%w", ErrTelegramEmptyAPIKey)
	}

	if len(cfg.ChatIDs) == 0 {
		queueUndelivered(ctx, eDB, TelegramQueueName, ch)

		return fmt.Errorf("%w", ErrTelegramEmptyUserIDs)
	}

	err := telegramInit(cfg.Token)
	if err != nil {
		// bot.New performs a network getMe call, so this path is reachable on
		// transient failures. Events are already dedup-flagged; queue them or
		// they are lost forever.
		queueUndelivered(ctx, eDB, TelegramQueueName, ch)

		return err
	}

	logger.Debug().Msgf("Started Telegram messenger (%v)", TelegramVersion)

	rl := ratelimit.New(TelegramAPILimit, ratelimit.Per(TelegramWindow))

	// Resend queued failures first. Rows are only removed after processing, so
	// a crash mid-loop re-delivers instead of losing; on shutdown, unprocessed
	// rows simply stay queued for the next run.
	for _, q := range queue.FetchFailedMsgs(ctx, eDB, TelegramQueueName) {
		if ctx.Err() != nil {
			break
		}

		processTelegram(ctx, eDB, q.Msg, cfg.ChatIDs, rl, cfg.Retries)

		// Failures were re-queued by processTelegram; drop the original row.
		queue.Dequeue(ctx, eDB, q.Key)
	}

	// Drain fully; processTelegram durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		processTelegram(ctx, eDB, g, cfg.ChatIDs, rl, cfg.Retries)
	}

	return nil
}

// markTelegramPermanent marks permanent errors as unrecoverable so retry-go
// stops retrying: Forbidden, BadRequest, Unauthorized, NotFound, Conflict, and
// MigrateError (chat upgraded to supergroup — the old ChatID is dead).
// TooManyRequests and network errors stay transient.
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

// processTelegram renders g as HTML and sends it to each chat ID, re-queueing
// on partial or total failure. Recipients already in SkipRecipients are omitted.
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

// telegramInit lazily creates the shared Telegram client (idempotent).
// bot.New validates the token via a network getMe call.
//
// No Start(): its getUpdates long-poll is only for receiving, which a
// send-only bot never consumes. SendMessage works without it.
func telegramInit(apiKey string) error {
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
	}

	return nil
}
