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
	"github.com/dkorunic/e-dnevnik-bot/format"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

const (
	DiscordSendDelay = 50 * time.Millisecond // recommended delay between messages

)

var (
	ErrDiscordEmptyAPIKey     = errors.New("empty Discord API key")
	ErrDiscordEmptyUserIds    = errors.New("empty list of Discord User IDs")
	ErrDiscordCreatingSession = errors.New("error creating Discord session")
	ErrDiscordCreatingChannel = errors.New("error creating Discord channel")
	ErrDiscordSendingMessage  = errors.New("error sending Discord message")
)

// Discord messenger processes events from a channel and attempts to communicate to one or more UserIDs, optionally
// returning an error.
func Discord(ctx context.Context, ch <-chan interface{}, token string, userIDs []string, retries uint) error {
	if token == "" {
		return fmt.Errorf("%w", ErrDiscordEmptyAPIKey)
	}

	if len(userIDs) == 0 {
		return fmt.Errorf("%w", ErrDiscordEmptyUserIds)
	}

	// create a Discord session
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		logger.Error().Msgf("%v: %v", ErrDiscordCreatingSession, err)

		return err
	}

	// create a Discord websocket
	err = dg.Open()
	if err != nil {
		return err
	}
	defer dg.Close()

	logger.Debug().Msg("Sending a message through Discord")

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

			// format message as rich message with embedded data
			fields := make([]*discordgo.MessageEmbedField, 0)
			for ii := range g.Fields {
				fields = append(fields, &discordgo.MessageEmbedField{
					Name:   g.Descriptions[ii],
					Value:  g.Fields[ii],
					Inline: true,
				})
			}

			sb := &strings.Builder{}
			format.PlainFormatSubject(sb, g.Username, g.Subject, g.IsExam)

			msg := &discordgo.MessageEmbed{
				Title:  sb.String(),
				Fields: fields,
			}

			// send to all recipients
			for _, u := range userIDs {
				// create a new user/private channel if needed
				c, err := dg.UserChannelCreate(u)
				if err != nil {
					logger.Error().Msgf("%v: %v", ErrDiscordCreatingChannel, err)

					break
				}

				// retryable and cancellable attempt to send a message
				err = retry.Do(
					func() error {
						_, err := dg.ChannelMessageSendEmbed(c.ID, msg)

						return err
					},
					retry.Attempts(retries),
					retry.Context(ctx),
				)
				if err != nil {
					logger.Error().Msgf("%v: %v", ErrDiscordSendingMessage, err)

					break
				}

				time.Sleep(DiscordSendDelay)
			}
		}
	}

	return err
}
