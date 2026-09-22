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
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/internal/version"
	"go.uber.org/ratelimit"
)

const (
	DiscordAPILimit = 10 // 10 API req/min per user/IP
	DiscordWindow   = 1 * time.Minute
	DiscordMinDelay = DiscordWindow / DiscordAPILimit
	DiscordQueue    = "discord-queue"

	// DiscordMaxTitleChars and the caps below are Discord's embed limits:
	// exceeding any one rejects the whole message.
	// https://discord.com/developers/docs/resources/channel#embed-object-embed-limits
	DiscordMaxTitleChars     = 256
	DiscordMaxFieldNameChars = 256
	DiscordMaxFieldValChars  = 1024
	DiscordMaxFields         = 25

	// Minimum value budget for a field: ellipsis plus content, so the last one
	// carries more than just "...".
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
	discordCreds     credGuard         // credentials discordCli was built from
	discordMu        sync.Mutex        // guards discordCli, discordChannels and discordCreds
	DiscordVersion   = version.ReadVersion("github.com/bwmarrin/discordgo")
)

// DiscordConfig holds the per-messenger settings for the Discord backend.
type DiscordConfig struct {
	Token   string
	UserIDs []string
	Retries uint
}

// Discord resends queued failures, then delivers ch to the configured user IDs.
// On init failure it drains ch to the queue rather than lose flagged events.
func Discord(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg DiscordConfig) (err error) {
	// Panic guard; stays nil on the resend path (see recoverMessenger).
	var inflight *msgtypes.Message

	defer func() {
		if r := recover(); r != nil {
			err = recoverMessenger(ctx, eDB, DiscordQueueName, ch, r, inflight)
		}
	}()

	if cfg.Token == "" {
		queueUndelivered(ctx, eDB, DiscordQueueName, ch)

		return fmt.Errorf("%w", ErrDiscordEmptyAPIKey)
	}

	if len(cfg.UserIDs) == 0 {
		queueUndelivered(ctx, eDB, DiscordQueueName, ch)

		return fmt.Errorf("%w", ErrDiscordEmptyUserIDs)
	}

	err = discordInit(cfg.Token)
	if err != nil {
		// Already dedup-flagged: queue them or lose them forever.
		queueUndelivered(ctx, eDB, DiscordQueueName, ch)

		return err
	}

	logger.Debug().Msgf("Started Discord messenger (%v)", DiscordVersion)

	rl := ratelimit.New(DiscordAPILimit, ratelimit.Per(DiscordWindow))

	resendQueued(ctx, eDB, DiscordQueueName, func(m msgtypes.Message) {
		processDiscord(ctx, eDB, m, cfg.UserIDs, rl, cfg.Retries)
	})

	// Drain fully; processDiscord durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		processDiscord(ctx, eDB, g, cfg.UserIDs, rl, cfg.Retries)
		inflight = nil
	}

	return nil
}

// markDiscordPermanent stops retry-go on a permanent 4xx REST error. Transport
// errors keep their retry budget.
func markDiscordPermanent(err error) error {
	if err == nil {
		return nil
	}

	var rerr *discordgo.RESTError
	if errors.As(err, &rerr) && rerr.Response != nil {
		if isPermanentHTTPStatus(rerr.Response.StatusCode) {
			// Inner sentinel survives retry.Do's marker stripping.
			return retry.Unrecoverable(permanentError{err})
		}
	}

	return err
}

// discordEmbedFields renders g's pairs within Discord's per-field, per-name and
// total-embed caps. title draws on the same total budget, hence the parameter.
func discordEmbedFields(g msgtypes.Message, title string) []*discordgo.MessageEmbedField {
	available := min(len(g.Fields), len(g.Descriptions))
	budget := DiscordMaxEmbedChars - utf8.RuneCountInString(title)

	fields := make([]*discordgo.MessageEmbedField, 0, min(available, DiscordMaxFields))

	droppedAt := -1
	truncatedValues := 0
	overCap := 0

	for ii := range available {
		// cellValues' alignment padding; every other backend skips it too.
		if g.Fields[ii] == "" {
			continue
		}

		// Counts emitted fields: bounding the loop index instead would spend slots
		// on padding and drop real values that had room.
		if len(fields) >= DiscordMaxFields {
			overCap++

			continue
		}

		name := truncateWithEllipsis(g.Descriptions[ii], DiscordMaxFieldNameChars)
		value := truncateWithEllipsis(g.Fields[ii], DiscordMaxFieldValChars)

		// An empty name 400s, poison-dropping the whole alert.
		if name == "" {
			name = "-"
		}

		nameLen := utf8.RuneCountInString(name)
		valueLen := utf8.RuneCountInString(value)

		// Needs real room: Discord rejects an empty Value, and a bare ellipsis is
		// useless.
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

	if overCap > 0 {
		logger.Debug().Msgf("Discord: dropped %d field(s) past the per-embed field cap (%d)",
			overCap, DiscordMaxFields)
	}

	if droppedAt >= 0 {
		logger.Debug().Msgf("Discord: embed total-size cap (%d) reached at field %d of %d",
			DiscordMaxEmbedChars, droppedAt, available)
	}

	if truncatedValues > 0 {
		logger.Debug().Msgf("Discord: truncated %d field value(s) to fit embed total-size cap (%d)",
			truncatedValues, DiscordMaxEmbedChars)
	}

	return fields
}

// processDiscord sends g as a capped embed to each user ID over a lazily
// resolved, cached DM channel, re-queueing on partial or total failure.
func processDiscord(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, userIDs []string, rl ratelimit.Limiter, retries uint) {
	title := truncateWithEllipsis(format.PlainSubject(g.Username, g.Subject, g.Code), DiscordMaxTitleChars)

	fields := discordEmbedFields(g, title)

	msg := discordgo.MessageEmbed{
		Title:  title,
		Fields: fields,
	}

	run := newRecipientRun(g)

	for _, u := range userIDs {
		if run.skipped(u) {
			continue
		}

		// Before rl.Take(), so shutdown is not blocked on a token.
		if ctx.Err() != nil {
			run.interrupt()

			break
		}

		rl.Take()

		// Resolved lazily, cached across recipients.
		discordMu.Lock()
		channelID, cached := discordChannels[u]
		discordMu.Unlock()

		if !cached {
			c, err := discordCli.UserChannelCreate(u,
				discordgo.WithContext(ctx),
				discordgo.WithRetryOnRatelimit(true),
				discordgo.WithRestRetries(1))
			if err != nil {
				if isPermanentSendErr(markDiscordPermanent(err)) {
					// Invalid or unknown user: drop, don't requeue.
					logger.Error().Msgf("%v: permanently dropping recipient %q: %v", ErrDiscordCreatingChannel, u, err)

					run.poison(u)

					continue
				}

				logger.Error().Msgf("%v: %v", ErrDiscordCreatingChannel, err)

				run.failed()

				continue
			}

			channelID = c.ID
			discordMu.Lock()
			discordChannels[u] = channelID
			discordMu.Unlock()
		}

		err := sendDiscordEmbed(ctx, channelID, &msg, retries)
		if err != nil && isPermanentSendErr(err) && cached {
			// A cached DM channel goes stale when the user closes the DM or it
			// is deleted: evict, re-resolve and resend once before classifying.
			// A failed re-resolve keeps the original error; the cache is already
			// evicted, so the next cycle starts clean.
			discordMu.Lock()
			delete(discordChannels, u)
			discordMu.Unlock()

			logger.Warn().Msgf("Discord: cached DM channel for %q rejected, re-resolving: %v", u, err)

			if c, cerr := discordCli.UserChannelCreate(u,
				discordgo.WithContext(ctx),
				discordgo.WithRetryOnRatelimit(true),
				discordgo.WithRestRetries(1)); cerr != nil {
				logger.Error().Msgf("%v: %v", ErrDiscordCreatingChannel, cerr)
			} else {
				discordMu.Lock()
				discordChannels[u] = c.ID
				discordMu.Unlock()

				err = sendDiscordEmbed(ctx, c.ID, &msg, retries)
			}
		}

		if err != nil {
			if isPermanentSendErr(err) {
				// Blocked bot or unknown channel: drop, don't requeue.
				logger.Error().Msgf("%v: permanently dropping recipient %q: %v", ErrDiscordSendingMessage, u, err)

				run.poison(u)

				continue
			}

			logger.Error().Msgf("%v: %v", ErrDiscordSendingMessage, err)

			run.failed()

			continue
		}

		run.delivered(u)
	}

	run.finish(ctx, eDB, DiscordQueueName, g)
}

// sendDiscordEmbed delivers msg under the per-recipient retry budget, marking
// permanent 4xx failures for the caller to poison-drop.
func sendDiscordEmbed(ctx context.Context, channelID string, msg *discordgo.MessageEmbed, retries uint) error {
	return retry.New(
		retry.Attempts(retries),
		retry.Context(ctx),
		retry.Delay(DiscordMinDelay),
	).Do(
		func() error {
			_, err := discordCli.ChannelMessageSendEmbed(channelID,
				msg,
				discordgo.WithContext(ctx),
				discordgo.WithRetryOnRatelimit(true),
				discordgo.WithRestRetries(1))

			return markDiscordPermanent(err)
		},
	)
}

// discordInit lazily builds the shared REST-only client, rebuilding on a token
// change (see credGuard).
func discordInit(token string) error {
	discordMu.Lock()
	defer discordMu.Unlock()

	// No Open(): sending is pure REST and needs no gateway websocket. Holding
	// one added heartbeat and reconnect churn for nothing, and a failed Open()
	// left a half-initialised session that was never retried, discordCli already
	// being non-nil.
	if discordCli != nil && !discordCreds.changed(token) {
		return nil
	}

	logger.Debug().Msg("Initializing Discord client")

	cli, err := discordgo.New("Bot " + token)
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrDiscordCreatingSession, err)

		return err
	}

	cli.ShouldRetryOnRateLimit = true
	cli.MaxRestRetries = 1

	// Publish only on success, so a failed init retries next cycle. The channel
	// cache goes with the client: a DM channel belongs to the identity that
	// opened it.
	discordCli = cli
	discordChannels = make(map[string]string)
	discordCreds.record(token)

	return nil
}
