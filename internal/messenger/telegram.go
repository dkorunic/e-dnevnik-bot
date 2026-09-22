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
	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
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
	telegramCreds     credGuard  // credentials telegramCli was built from
	telegramMu        sync.Mutex // guards telegramCli and telegramCreds
	TelegramVersion   = version.ReadVersion("github.com/go-telegram/bot")

	telegramMigratedIDsMu sync.Mutex            // guards telegramMigratedIDs
	telegramMigratedIDs   = map[string]string{} // supergroup remaps seen this process: old chat ID -> new chat ID
)

// TelegramConfig holds the per-messenger settings for the Telegram backend.
type TelegramConfig struct {
	Token   string
	ChatIDs []string
	Retries uint
}

// Telegram resends queued failures, then delivers ch to the configured chat
// IDs. On init failure it drains ch to the queue rather than lose flagged
// events.
func Telegram(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg TelegramConfig) (err error) {
	// Panic guard; stays nil on the resend path (see recoverMessenger).
	var inflight *msgtypes.Message

	defer func() {
		if r := recover(); r != nil {
			err = recoverMessenger(ctx, eDB, TelegramQueueName, ch, r, inflight)
		}
	}()

	if cfg.Token == "" {
		queueUndelivered(ctx, eDB, TelegramQueueName, ch)

		return fmt.Errorf("%w", ErrTelegramEmptyAPIKey)
	}

	if len(cfg.ChatIDs) == 0 {
		queueUndelivered(ctx, eDB, TelegramQueueName, ch)

		return fmt.Errorf("%w", ErrTelegramEmptyUserIDs)
	}

	err = telegramInit(cfg.Token)
	if err != nil {
		// bot.New makes a network getMe call, so this is reachable on transient
		// failures. Already dedup-flagged: queue them or lose them forever.
		queueUndelivered(ctx, eDB, TelegramQueueName, ch)

		return err
	}

	logger.Debug().Msgf("Started Telegram messenger (%v)", TelegramVersion)

	rl := ratelimit.New(TelegramAPILimit, ratelimit.Per(TelegramWindow))

	resendQueued(ctx, eDB, TelegramQueueName, func(m msgtypes.Message) {
		processTelegram(ctx, eDB, m, cfg.ChatIDs, rl, cfg.Retries)
	})

	// Drain fully; processTelegram durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		processTelegram(ctx, eDB, g, cfg.ChatIDs, rl, cfg.Retries)
		inflight = nil
	}

	return nil
}

// markTelegramPermanent stops retry-go on Forbidden, BadRequest, Unauthorized,
// NotFound, Conflict and MigrateError — a migrated chat's old ID is dead.
// TooManyRequests and network errors stay transient.
func markTelegramPermanent(err error) error {
	if err == nil {
		return nil
	}

	// errors.As survives wrapping by future library versions.
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
		// Inner sentinel survives retry.Do's marker stripping.
		return retry.Unrecoverable(permanentError{err})
	}

	return err
}

// processTelegram sends g as HTML to each chat ID, re-queueing on partial or
// total failure.
func processTelegram(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, chatIDs []string, rl ratelimit.Limiter, retries uint) {
	// Trim before formatting, so <b>/<pre> stay balanced.
	m := truncateHTMLBody(g.Username, g.Subject, g.Code, g.Descriptions, g.Fields, TelegramMaxMessageChars)

	run := newRecipientRun(g)

	for _, origID := range chatIDs {
		if run.skipped(origID) {
			continue
		}

		// Follow migrations seen in earlier cycles. Bookkeeping stays on
		// origID so retries dedupe correctly.
		u := origID

		telegramMigratedIDsMu.Lock()
		if newID, ok := telegramMigratedIDs[origID]; ok {
			u = newID
		}
		telegramMigratedIDsMu.Unlock()

		uu, err := strconv.ParseInt(u, 10, 64)
		if err != nil {
			// Never becomes valid: drop loudly rather than skip silently.
			logger.Error().Msgf("%v: permanently dropping recipient %q: %v", ErrTelegramInvalidChatID, u, err)

			run.poison(origID)

			continue
		}

		msg := bot.SendMessageParams{
			ChatID:    uu,
			Text:      m,
			ParseMode: models.ParseModeHTML,
		}

		// Before rl.Take(), so shutdown is not blocked on a token.
		if ctx.Err() != nil {
			run.interrupt()

			break
		}

		rl.Take()

		err = sendTelegramMsg(ctx, &msg, retries)
		if err != nil {
			if mig, ok := errors.AsType[*bot.MigrateError](err); ok {
				// Upgraded to a supergroup: remap, persist the new ID, and
				// deliver there now.
				newID := strconv.Itoa(mig.MigrateToChatID)

				logger.Warn().Msgf("Telegram: chat %v was migrated to supergroup %v — remapping and updating configuration",
					u, mig.MigrateToChatID)

				telegramMigratedIDsMu.Lock()
				telegramMigratedIDs[origID] = newID
				telegramMigratedIDsMu.Unlock()

				telegramPersistChatID(ctx, origID, newID)

				msg.ChatID = int64(mig.MigrateToChatID)
				err = sendTelegramMsg(ctx, &msg, retries)
			}
		}

		if err != nil {
			if isPermanentSendErr(err) {
				// Blocked bot or deleted chat: drop, don't requeue.
				logger.Error().Msgf("%v: permanently dropping recipient %q: %v", ErrTelegramSendingMessage, u, err)

				run.poison(origID)

				continue
			}

			logger.Error().Msgf("%v: %v", ErrTelegramSendingMessage, err)

			run.failed()

			continue
		}

		run.delivered(origID)
	}

	run.finish(ctx, eDB, TelegramQueueName, g)
}

// sendTelegramMsg delivers msg under the per-recipient retry budget, marking
// permanent failures for the caller to poison-drop or remap.
func sendTelegramMsg(ctx context.Context, msg *bot.SendMessageParams, retries uint) error {
	return retry.New(
		retry.Attempts(retries),
		retry.Context(ctx),
		retry.Delay(TelegramMinDelay),
	).Do(
		func() error {
			_, err := telegramCli.SendMessage(ctx, msg)

			return markTelegramPermanent(err)
		},
	)
}

// telegramPersistChatID rewrites a migrated chat ID so the remap survives a
// restart. Best effort — the in-process remap holds regardless, and a failure
// only costs a manual edit.
func telegramPersistChatID(ctx context.Context, oldID, newID string) {
	confFile, ok := ctx.Value(ConfFileKey).(string)
	if !ok {
		return
	}

	configRewriteMu.Lock()
	defer configRewriteMu.Unlock()

	if !isWriteable(confFile) {
		logger.Warn().Msgf("Telegram: cannot persist chat ID remap %v -> %v: %q is not writable", oldID, newID, confFile)

		return
	}

	// Non-validating: LoadConfig's fail-fast validators would os.Exit this
	// goroutine on a broken mid-run edit.
	cfg, err := config.LoadConfigRaw(confFile)
	if err != nil {
		logger.Error().Msgf("Telegram: unable to load configuration for chat ID remap: %v", err)

		return
	}

	found := false

	for i, id := range cfg.Telegram.ChatIDs {
		if id == oldID {
			cfg.Telegram.ChatIDs[i] = newID
			found = true
		}
	}

	if !found {
		return
	}

	if err := config.SaveConfig(confFile, cfg); err != nil {
		logger.Error().Msgf("Telegram: unable to save chat ID remap to configuration: %v", err)

		return
	}

	logger.Info().Msgf("Telegram: persisted chat ID remap %v -> %v to %q", oldID, newID, confFile)
}

// telegramInit lazily builds the shared client, rebuilding on a token change
// (see credGuard). bot.New validates the token over the network, so an
// unchanged token must not reach it.
//
// No Start(): its getUpdates long-poll only receives, which a send-only bot
// never consumes.
func telegramInit(apiKey string) error {
	telegramMu.Lock()
	defer telegramMu.Unlock()

	if telegramCli != nil && !telegramCreds.changed(apiKey) {
		return nil
	}

	logger.Debug().Msg("Initializing Telegram client")

	// Build locally, then publish: assigning directly would nil a working
	// client whenever the getMe below fails.
	cli, err := bot.New(apiKey)
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrTelegramSession, err)

		return err
	}

	telegramCli = cli
	telegramCreds.record(apiKey)

	return nil
}
