// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/avast/retry-go/v5"
	"github.com/bwmarrin/discordgo"
	"github.com/dkorunic/e-dnevnik-bot/internal/format"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/internal/version"
	"go.uber.org/ratelimit"
)

const (
	DiscordAPILimit = 10 // 10 API req/min per user/IP
	DiscordWindow   = 1 * time.Minute
	DiscordMinDelay = DiscordWindow / DiscordAPILimit
	DiscordQueue    = "discord-queue"

	// Discord embed size limits; exceeding any rejects the whole message, so we truncate client-side.
	// See https://discord.com/developers/docs/resources/channel#embed-object-embed-limits.
	DiscordMaxTitleChars     = 256
	DiscordMaxFieldNameChars = 256
	DiscordMaxFieldValChars  = 1024
	DiscordMaxFields         = 25

	// minDiscordFieldValueRunes is the minimum value budget when appending a field.
	// Must be ≥ 3 (ellipsis) plus content so the last field carries more than just "...".
	minDiscordFieldValueRunes = 16
)

var (
	ErrDiscordEmptyAPIKey     = errors.New("empty Discord API key")
	ErrDiscordEmptyUserIDs    = errors.New("empty list of Discord User IDs")
	ErrDiscordCreatingSession = errors.New("error creating Discord session")
	ErrDiscordCreatingChannel = errors.New("error creating Discord channel")
	ErrDiscordSendingMessage  = errors.New("error sending Discord message")

	DiscordQueueName = []byte(DiscordQueue)
	discordCli       *discordgo.Session
	discordChannels  map[string]string // cached DM channel IDs per user ID
	discordMu        sync.Mutex        // guards discordCli and discordChannels initialisation
	DiscordVersion   = version.ReadVersion("github.com/bwmarrin/discordgo")
)

// DiscordConfig holds the per-messenger settings for the Discord backend.
type DiscordConfig struct {
	Token   string
	UserIDs []string
	Retries uint
}

// Discord resends any queued failures, then delivers live messages from ch to
// the configured user IDs. On init failure it drains ch into the queue so
// already-dedup-flagged events are not lost.
func Discord(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg DiscordConfig) error {
	if cfg.Token == "" {
		queueUndelivered(ctx, eDB, DiscordQueueName, ch)

		return fmt.Errorf("%w", ErrDiscordEmptyAPIKey)
	}

	if len(cfg.UserIDs) == 0 {
		queueUndelivered(ctx, eDB, DiscordQueueName, ch)

		return fmt.Errorf("%w", ErrDiscordEmptyUserIDs)
	}

	err := discordInit(cfg.Token)
	if err != nil {
		// Events are already dedup-flagged; queue them or they are lost forever.
		queueUndelivered(ctx, eDB, DiscordQueueName, ch)

		return err
	}

	logger.Debug().Msgf("Started Discord messenger (%v)", DiscordVersion)

	rl := ratelimit.New(DiscordAPILimit, ratelimit.Per(DiscordWindow))

	// Resend queued failures first. Rows are only removed after processing, so
	// a crash mid-loop re-delivers instead of losing; on shutdown, unprocessed
	// rows simply stay queued for the next run.
	for _, q := range queue.FetchFailedMsgs(ctx, eDB, DiscordQueueName) {
		if ctx.Err() != nil {
			break
		}

		processDiscord(ctx, eDB, q.Msg, cfg.UserIDs, rl, cfg.Retries)

		// Failures were re-queued by processDiscord; drop the original row.
		queue.Dequeue(ctx, eDB, q.Key)
	}

	// Drain fully; processDiscord durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		processDiscord(ctx, eDB, g, cfg.UserIDs, rl, cfg.Retries)
	}

	return nil
}

// markDiscordPermanent marks permanent 4xx REST errors (except 408/429) as
// unrecoverable so retry-go stops retrying. Non-REST/transport errors keep
// their normal retry budget.
func markDiscordPermanent(err error) error {
	if err == nil {
		return nil
	}

	var rerr *discordgo.RESTError
	if errors.As(err, &rerr) && rerr.Response != nil {
		if isPermanentHTTPStatus(rerr.Response.StatusCode) {
			return retry.Unrecoverable(err)
		}
	}

	return err
}

// processDiscord renders g as an embed (field count and sizes capped to
// Discord's limits) and sends it to each user ID via a lazily-resolved,
// cached DM channel, re-queueing on partial or total failure. Recipients
// already in SkipRecipients are omitted.
func processDiscord(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, userIDs []string, rl ratelimit.Limiter, retries uint) {
	// Cap field count and truncate strings so Discord does not reject the embed.
	available := min(len(g.Fields), len(g.Descriptions))
	count := min(available, DiscordMaxFields)

	if available > count {
		logger.Debug().Msgf("Discord: dropping %d of %d fields to fit per-embed field cap (%d)",
			available-count, available, DiscordMaxFields)
	}

	title := truncateWithEllipsis(format.PlainSubject(g.Username, g.Subject, g.Code), DiscordMaxTitleChars)

	// Total-embed-chars cap rejects otherwise-valid messages.
	budget := DiscordMaxEmbedChars - utf8.RuneCountInString(title)

	fields := make([]*discordgo.MessageEmbedField, 0, count)

	droppedAt := -1
	truncatedValues := 0

	for ii := range count {
		name := truncateWithEllipsis(g.Descriptions[ii], DiscordMaxFieldNameChars)
		value := truncateWithEllipsis(g.Fields[ii], DiscordMaxFieldValChars)

		nameLen := utf8.RuneCountInString(name)
		valueLen := utf8.RuneCountInString(value)

		// Require real value room: Discord rejects empty Value and bare ellipses are useless.
		if nameLen+minDiscordFieldValueRunes > budget {
			droppedAt = ii

			break
		}

		if nameLen+valueLen > budget {
			value = truncateWithEllipsis(value, budget-nameLen)
			valueLen = utf8.RuneCountInString(value)
			truncatedValues++
		}

		budget -= nameLen + valueLen

		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   name,
			Value:  value,
			Inline: true,
		})
	}

	if droppedAt >= 0 {
		logger.Debug().Msgf("Discord: embed total-size cap (%d) reached; dropped %d of %d fields",
			DiscordMaxEmbedChars, count-droppedAt, count)
	}

	if truncatedValues > 0 {
		logger.Debug().Msgf("Discord: truncated %d field value(s) to fit embed total-size cap (%d)",
			truncatedValues, DiscordMaxEmbedChars)
	}

	msg := discordgo.MessageEmbed{
		Title:  title,
		Fields: fields,
	}

	skipSet := make(map[string]struct{}, len(g.SkipRecipients))
	for _, r := range g.SkipRecipients {
		skipSet[r] = struct{}{}
	}

	var successfulIDs []string

	anyFailed := false
	// Tracks incomplete loops so shutdown-cancelled sends get re-queued, not dropped.
	allProcessed := true

	for _, u := range userIDs {
		if _, skip := skipSet[u]; skip {
			continue
		}

		// Check before rl.Take() so shutdown is not blocked on a token.
		if ctx.Err() != nil {
			allProcessed = false

			break
		}

		rl.Take()

		// Resolve DM channel lazily; cache across recipients.
		discordMu.Lock()
		channelID, ok := discordChannels[u]
		discordMu.Unlock()

		var err error

		if !ok {
			var c *discordgo.Channel

			c, err = discordCli.UserChannelCreate(u,
				discordgo.WithContext(ctx),
				discordgo.WithRetryOnRatelimit(true),
				discordgo.WithRestRetries(1))
			if err != nil {
				logger.Error().Msgf("%v: %v", ErrDiscordCreatingChannel, err)

				anyFailed = true

				continue
			}

			channelID = c.ID
			discordMu.Lock()
			discordChannels[u] = channelID
			discordMu.Unlock()
		}

		err = retry.New(
			retry.Attempts(retries),
			retry.Context(ctx),
			retry.Delay(DiscordMinDelay),
		).Do(
			func() error {
				_, err := discordCli.ChannelMessageSendEmbed(channelID,
					&msg,
					discordgo.WithContext(ctx),
					discordgo.WithRetryOnRatelimit(true),
					discordgo.WithRestRetries(1))

				return markDiscordPermanent(err)
			},
		)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrDiscordSendingMessage, err)

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
		if err := queue.StoreFailedMsgs(sctx, eDB, DiscordQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()
	}
}

// discordInit lazily creates the shared REST-only Discord client (idempotent).
func discordInit(token string) error {
	discordMu.Lock()
	defer discordMu.Unlock()

	var err error

	// No Open(): this bot only sends via REST (UserChannelCreate /
	// ChannelMessageSendEmbed), which needs no gateway websocket. Keeping a
	// gateway connection open added heartbeats/reconnect churn for nothing —
	// and a failed Open() left a half-initialized session that was never
	// retried because discordCli was already non-nil.
	if discordCli == nil {
		logger.Debug().Msg("Initializing Discord client")

		discordCli, err = discordgo.New("Bot " + token)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrDiscordCreatingSession, err)

			return err
		}

		discordCli.ShouldRetryOnRateLimit = true
		discordCli.MaxRestRetries = 1

		discordChannels = make(map[string]string)
	}

	return nil
}
