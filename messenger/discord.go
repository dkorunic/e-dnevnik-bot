// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/avast/retry-go/v5"
	"github.com/bwmarrin/discordgo"
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/queue"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/version"
	"go.uber.org/ratelimit"
)

const (
	DiscordAPILimit = 10 // 10 API req/min per user/IP
	DiscordWindow   = 1 * time.Minute
	DiscordMinDelay = DiscordWindow / DiscordAPILimit
	DiscordQueue    = "discord-queue"

	// Discord embed size limits from
	// https://discord.com/developers/docs/resources/channel#embed-object-embed-limits.
	// Exceeding any of these causes the API to reject the entire message, so
	// we truncate client-side.
	DiscordMaxTitleChars     = 256
	DiscordMaxFieldNameChars = 256
	DiscordMaxFieldValChars  = 1024
	DiscordMaxFields         = 25

	// minDiscordFieldValueRunes is the smallest value budget we are willing to
	// accept when deciding whether to keep appending fields to an embed. It
	// must be ≥ 3 so truncateWithEllipsis can emit its ellipsis; values larger
	// than 3 guarantee the last emitted field carries some actual content in
	// addition to the ellipsis, rather than just "...".
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

// Discord sends messages through the Discord API.
//
// It takes the following parameters:
// - ctx: the context.Context object for handling deadlines and cancellations.
// - eDB: the database instance for checking failed messages.
// - ch: a channel for receiving messages to be sent.
// - token: the Discord API key.
// - userIDs: a slice of strings containing the IDs of the recipients.
// - retries: the number of times to retry sending a message in case of failure.
//
// It returns an error indicating any failures that occurred during the process.
func Discord(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, token string, userIDs []string, retries uint) error {
	if token == "" {
		return fmt.Errorf("%w", ErrDiscordEmptyAPIKey)
	}

	if len(userIDs) == 0 {
		return fmt.Errorf("%w", ErrDiscordEmptyUserIDs)
	}

	err := discordInit(token)
	if err != nil {
		return err
	}

	logger.Debug().Msgf("Started Discord messenger (%v)", DiscordVersion)

	rl := ratelimit.New(DiscordAPILimit, ratelimit.Per(DiscordWindow))

	// Drain queued failures first; re-queue tail on shutdown.
	failedMsgs := queue.FetchFailedMsgs(ctx, eDB, DiscordQueueName)
	for i, g := range failedMsgs {
		if ctx.Err() != nil {
			queue.RequeueMsgs(eDB, DiscordQueueName, failedMsgs[i:])

			return ctx.Err()
		}

		processDiscord(ctx, eDB, g, userIDs, rl, retries)

		if ctx.Err() != nil {
			queue.RequeueMsgs(eDB, DiscordQueueName, failedMsgs[i+1:])

			return ctx.Err()
		}
	}

	for g := range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processDiscord(ctx, eDB, g, userIDs, rl, retries)
		}
	}

	return nil
}

// markDiscordPermanent wraps Discord API errors that will never succeed on
// retry in retry.Unrecoverable so retry-go short-circuits the remaining
// attempts. 4xx responses (auth failure, unknown channel, malformed embed) are
// permanent — except 408 (timeout) and 429 (rate-limit), which are transient.
// Non-REST errors (transport/network) fall through with their normal retry
// budget.
func markDiscordPermanent(err error) error {
	if err == nil {
		return nil
	}

	var rerr *discordgo.RESTError
	if errors.As(err, &rerr) && rerr.Response != nil {
		code := rerr.Response.StatusCode
		if code >= 400 && code < 500 && code != 408 && code != 429 {
			return retry.Unrecoverable(err)
		}
	}

	return err
}

// processDiscord processes a message from a channel and sends it to the specified user IDs on Discord.
//
// It takes the following parameters:
// - ctx: the context.Context object for managing the execution of the function
// - eDB: the database instance for checking failed messages
// - g: the message to be processed
// - userIDs: the list of Discord user IDs to send the messages to
// - rl: the rate limiter
// - retries: the number of retry attempts to send the message
//
// It returns no value and has no side effects except for error logging.
func processDiscord(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, userIDs []string, rl ratelimit.Limiter, retries uint) {
	// Cap field count and truncate strings so Discord does not reject the embed.
	available := min(len(g.Fields), len(g.Descriptions))
	count := min(available, DiscordMaxFields)

	if available > count {
		logger.Debug().Msgf("Discord: dropping %d of %d fields to fit per-embed field cap (%d)",
			available-count, available, DiscordMaxFields)
	}

	sb := strings.Builder{}
	sb.Grow(len(g.Username) + len(g.Subject) + 40)
	format.PlainFormatSubject(&sb, g.Username, g.Subject, g.Code)

	title := truncateWithEllipsis(sb.String(), DiscordMaxTitleChars)

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

// discordInit initializes a Discord client and starts a session if it has not been initialized yet.
//
// The function takes a string parameter, the Discord API key, and returns an error if there was a problem creating the client or starting the session.
// If the client has already been initialized, the function does nothing and returns nil.
//
// The function logs errors if there was a problem creating the client or starting the session.
func discordInit(token string) error {
	discordMu.Lock()
	defer discordMu.Unlock()

	var err error

	if discordCli == nil {
		logger.Debug().Msg("Initializing Discord client")

		discordCli, err = discordgo.New("Bot " + token)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrDiscordCreatingSession, err)

			return err
		}

		discordCli.ShouldReconnectOnError = true
		discordCli.ShouldRetryOnRateLimit = true
		discordCli.MaxRestRetries = 1

		discordChannels = make(map[string]string)

		return discordCli.Open()
	}

	return nil
}
