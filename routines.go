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

package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/messenger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/scrape"
	"github.com/dustin/go-broadcast"
	"github.com/tj/go-spin"
)

const (
	broadcastBufLen    = 10                     // events to broadcast for sending at once
	spinnerRotateDelay = 100 * time.Millisecond // spinner delay
)

var (
	ErrScrapingUser = errors.New("error scraping data for user")
	ErrDiscord      = errors.New("Discord messenger issue")  //nolint:stylecheck
	ErrTelegram     = errors.New("Telegram messenger issue") //nolint:stylecheck
	ErrSlack        = errors.New("Slack messenger issue")    //nolint:stylecheck
	ErrMail         = errors.New("Mail messenger issue")     //nolint:stylecheck
)

// scrapers will call subjects/grades/exams scraping for every configured AAI/AOSI user and send grades/exams messages
// to a channel.
func scrapers(ctx context.Context, wgScrape *sync.WaitGroup, gradesScraped chan<- msgtypes.Message, config tomlConfig) {
	logger.Debug().Msg("Starting scrapers")

	for _, i := range config.User {
		wgScrape.Add(1)

		i := i

		go func() {
			defer wgScrape.Done()

			err := scrape.GetGradesAndEvents(ctx, gradesScraped, i.Username, i.Password, *retries)
			if err != nil {
				logger.Warn().Msgf("%v %v: %v", ErrScrapingUser, i.Username, err)
				exitWithError.Store(true)
			}
		}()
	}
}

// msgSend will process grades/exams messages and broadcast to one or more message services.
func msgSend(ctx context.Context, wgMsg *sync.WaitGroup, gradesMsg <-chan msgtypes.Message, config tomlConfig) {
	wgMsg.Add(1)

	go func() {
		defer wgMsg.Done()

		bcast := broadcast.NewBroadcaster(broadcastBufLen)
		defer bcast.Close()

		// Discord sender
		if config.discordEnabled {
			ch := make(chan interface{}) // broadcast listener
			defer close(ch)

			bcast.Register(ch) // broadcast registration
			defer bcast.Unregister(ch)

			wgMsg.Add(1)

			go func() {
				defer wgMsg.Done()
				logger.Debug().Msg("Discord messenger started")

				if err := messenger.Discord(ctx, ch, config.Discord.Token, config.Discord.UserIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrDiscord, err)
					exitWithError.Store(true)
				}
			}()
		}

		// Telegram sender
		if config.telegramEnabled {
			ch := make(chan interface{}) // broadcast listener
			defer close(ch)

			bcast.Register(ch) // broadcast registration
			defer bcast.Unregister(ch)

			wgMsg.Add(1)

			go func() {
				defer wgMsg.Done()
				logger.Debug().Msg("Telegram messenger started")

				if err := messenger.Telegram(ctx, ch, config.Telegram.Token, config.Telegram.ChatIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrTelegram, err)
					exitWithError.Store(true)
				}
			}()
		}

		// Slack sender
		if config.slackEnabled {
			ch := make(chan interface{}) // broadcast listener
			defer close(ch)

			bcast.Register(ch) // broadcast registration
			defer bcast.Unregister(ch)

			wgMsg.Add(1)

			go func() {
				defer wgMsg.Done()
				logger.Debug().Msg("Slack messenger started")

				if err := messenger.Slack(ctx, ch, config.Slack.Token, config.Slack.ChatIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrSlack, err)
					exitWithError.Store(true)
				}
			}()
		}

		// mail Sender
		if config.mailEnabled {
			ch := make(chan interface{}) // broadcast listener
			defer close(ch)

			bcast.Register(ch) // broadcast registration
			defer bcast.Unregister(ch)

			wgMsg.Add(1)

			go func() {
				defer wgMsg.Done()
				logger.Debug().Msg("Mail messenger started")

				if err := messenger.Mail(ctx, ch, config.Mail.Server, config.Mail.Port, config.Mail.Username, config.Mail.Password, config.Mail.From, config.Mail.Subject, config.Mail.To, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrMail, err)
					exitWithError.Store(true)
				}
			}()
		}

		// broadcast incoming messages
		for g := range gradesMsg {
			select {
			case <-ctx.Done():
				return
			default:
				bcast.Submit(g)
			}
		}
	}()
}

// msgDedup acts like a filter: processes all incoming messages, calls in to database check and if it hasn't been found
// and if it is not an initial run, it will pass through to messengers for further alerting.
func msgDedup(ctx context.Context, wgFilter *sync.WaitGroup, gradesScraped <-chan msgtypes.Message, gradesMsg chan<- msgtypes.Message) {
	wgFilter.Add(1)

	go func() {
		defer wgFilter.Done()

		// open KV store
		eDB, err := db.New(*dbFile)
		if err != nil {
			logger.Fatal().Msgf("Unable to open database: %v", err)
		}
		defer eDB.Close()

		if !eDB.Existing() {
			logger.Info().Msg("Newly initialized database, won't sent alerts in this run")
		}

		for g := range gradesScraped {
			select {
			case <-ctx.Done():
				return
			default:
				// check if it is an already known alert
				found, err := eDB.CheckAndFlag(g.Username, g.Subject, g.Fields)
				if err != nil {
					logger.Fatal().Msgf("Problem with database, cannot continue: %v", err)
				}

				// check if is the initial run and send only if not
				if !found && eDB.Existing() {
					logger.Info().Msgf("New alert for: %v/%v: %+v", g.Username, g.Subject, g)
					gradesMsg <- g
				}
			}
		}

		close(gradesMsg)
	}()
}

// spinner shows a spiffy terminal spinner while waiting endlessly.
func spinner() {
	s := spin.New()

	for {
		fmt.Printf("\rWaiting... %v", s.Next())
		time.Sleep(spinnerRotateDelay)
	}
}
