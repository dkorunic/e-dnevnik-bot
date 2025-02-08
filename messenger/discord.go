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
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/bwmarrin/discordgo"
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"go.uber.org/ratelimit"
)

const (
	DiscordAPILimit = 10 // 10 API req/min per user/IP
	DiscordWindow   = 1 * time.Minute
	DiscordMinDelay = DiscordWindow / DiscordAPILimit
	DiscordQueue    = "discord-queue"
)

var (
	ErrDiscordEmptyAPIKey     = errors.New("empty Discord API key")
	ErrDiscordEmptyUserIDs    = errors.New("empty list of Discord User IDs")
	ErrDiscordCreatingSession = errors.New("error creating Discord session")
	ErrDiscordCreatingChannel = errors.New("error creating Discord channel")
	ErrDiscordSendingMessage  = errors.New("error sending Discord message")

	DiscordQueueName = []byte(DiscordQueue)
	discordCli       *discordgo.Session
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
func Discord(ctx context.Context, eDB *db.Edb, ch <-chan msgtypes.Message, token string, userIDs []string, retries uint) error {
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

	logger.Debug().Msg("Started Discord messenger")

	rl := ratelimit.New(DiscordAPILimit, ratelimit.Per(DiscordWindow))

	var g msgtypes.Message

	// process all failed messages
	for _, g = range fetchFailedMsgs(eDB, DiscordQueueName) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processDiscord(ctx, eDB, g, userIDs, rl, retries)
		}
	}

	// process all messages
	for g = range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processDiscord(ctx, eDB, g, userIDs, rl, retries)
		}
	}

	return nil
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
func processDiscord(ctx context.Context, eDB *db.Edb, g msgtypes.Message, userIDs []string, rl ratelimit.Limiter, retries uint) {
	// format message as rich message with embedded data
	fields := make([]*discordgo.MessageEmbedField, 0)
	for ii := range g.Fields {
		fields = append(fields, &discordgo.MessageEmbedField{
			Name:   g.Descriptions[ii],
			Value:  g.Fields[ii],
			Inline: true,
		})
	}

	sb := strings.Builder{}
	format.PlainFormatSubject(&sb, g.Username, g.Subject, g.IsExam)

	msg := discordgo.MessageEmbed{
		Title:  sb.String(),
		Fields: fields,
	}

	// send to all recipients
	for _, u := range userIDs {
		rl.Take()

		// create a new user/private channel if needed
		c, err := discordCli.UserChannelCreate(u,
			discordgo.WithContext(ctx),
			discordgo.WithRetryOnRatelimit(true),
			discordgo.WithRestRetries(1))
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrDiscordCreatingChannel, err)

			break
		}

		// retryable and cancellable attempt to send a message
		err = retry.Do(
			func() error {
				_, err := discordCli.ChannelMessageSendEmbed(c.ID,
					&msg,
					discordgo.WithContext(ctx),
					discordgo.WithRetryOnRatelimit(true),
					discordgo.WithRestRetries(1))

				return err
			},
			retry.Attempts(retries),
			retry.Context(ctx),
			retry.Delay(DiscordMinDelay),
		)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrDiscordSendingMessage, err)

			// store failed message
			if err := storeFailedMsgs(eDB, DiscordQueueName, g); err != nil {
				logger.Error().Msgf("%v: %v", ErrQueueing, err)
			}

			continue
		}
	}
}

// discordInit initializes a Discord client and starts a session if it has not been initialized yet.
//
// The function takes a string parameter, the Discord API key, and returns an error if there was a problem creating the client or starting the session.
// If the client has already been initialized, the function does nothing and returns nil.
//
// The function logs errors if there was a problem creating the client or starting the session.
func discordInit(token string) error {
	var err error

	if discordCli == nil {
		// create a Discord session
		discordCli, err = discordgo.New("Bot " + token)
		if err != nil {
			logger.Error().Msgf("%v: %v", ErrDiscordCreatingSession, err)

			return err
		}

		discordCli.ShouldReconnectOnError = true
		discordCli.ShouldRetryOnRateLimit = true
		discordCli.MaxRestRetries = 1

		// create a Discord websocket
		return discordCli.Open()
	}

	return nil
}
