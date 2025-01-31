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

	"github.com/blang/semver/v4"
	"github.com/dkorunic/e-dnevnik-bot/config"
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/messenger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/scrape"
	"github.com/dustin/go-broadcast"
	"github.com/google/go-github/v68/github"
	"github.com/tj/go-spin"
)

const (
	broadcastBufLen    = 10                     // events to broadcast for sending at once
	spinnerRotateDelay = 100 * time.Millisecond // spinner delay
	githubOrg          = "dkorunic"
	githubRepo         = "e-dnevnik-bot"
)

var (
	ErrScrapingUser = errors.New("error scraping data for User")
	ErrDiscord      = errors.New("Discord messenger issue")  //nolint:stylecheck
	ErrTelegram     = errors.New("Telegram messenger issue") //nolint:stylecheck
	ErrSlack        = errors.New("Slack messenger issue")    //nolint:stylecheck
	ErrMail         = errors.New("Mail messenger issue")     //nolint:stylecheck
	ErrCalendar     = errors.New("Google Calendar issue")    //nolint:stylecheck
	ErrWhatsApp     = errors.New("WhatsApp issue")           //nolint:stylecheck

	formatHRDateOnly = "2.1."
)

// scrapers will call subjects/grades/exams scraping for every configured AAI/AOSI User and send grades/exams messages
// to a channel.
func scrapers(ctx context.Context, wgScrape *sync.WaitGroup, gradesScraped chan<- msgtypes.Message, cfg config.TomlConfig) {
	logger.Debug().Msg("Starting scrapers")

	for _, i := range cfg.User {
		wgScrape.Add(1)

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

// msgSend handles the distribution of scraped grades and events messages to various messaging services configured in the application.
// It sets up a broadcaster to relay messages to multiple services such as Discord, Telegram, Slack, Mail, Google Calendar, and WhatsApp.
// Each service runs in its own goroutine and listens to both live and previously failed messages from a database queue.
//
// Parameters:
// - ctx: the context for cancellation and timeout.
// - eDB: the database instance for checking failed messages.
// - wgMsg: a WaitGroup to synchronize the completion of message sending.
// - gradesMsg: a channel receiving messages to be sent to configured messengers.
// - cfg: the configuration settings containing enabled services and their respective credentials.
func msgSend(ctx context.Context, eDB *db.Edb, wgMsg *sync.WaitGroup, gradesMsg <-chan msgtypes.Message, cfg config.TomlConfig) {
	wgMsg.Add(1)

	go func() {
		defer wgMsg.Done()

		var wgFailedMsg sync.WaitGroup

		bcast := broadcast.NewBroadcaster(broadcastBufLen)
		defer bcast.Close()

		// Discord sender
		if cfg.DiscordEnabled {
			ch := make(chan interface{}) // broadcast listener
			defer close(ch)

			bcast.Register(ch) // broadcast registration
			defer bcast.Unregister(ch)

			// handle failed messages
			wgFailedMsg.Add(1)

			go func() {
				defer wgFailedMsg.Done()
				logger.Debug().Msg("Discord processing failed messages started")

				fetchAndSendFailedMsg(eDB, ch, messenger.DiscordQueueName)
			}()

			// handle regular messages
			wgMsg.Add(1)

			go func() {
				defer wgMsg.Done()
				logger.Debug().Msg("Discord messenger started")

				if err := messenger.Discord(ctx, eDB, ch, cfg.Discord.Token, cfg.Discord.UserIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrDiscord, err)
					exitWithError.Store(true)
				}
			}()
		}

		// Telegram sender
		if cfg.TelegramEnabled {
			ch := make(chan interface{}) // broadcast listener
			defer close(ch)

			bcast.Register(ch) // broadcast registration
			defer bcast.Unregister(ch)

			// handle failed messages
			wgFailedMsg.Add(1)

			go func() {
				defer wgFailedMsg.Done()
				logger.Debug().Msg("Telegram processing failed messages started")

				fetchAndSendFailedMsg(eDB, ch, messenger.TelegramQueueName)
			}()

			// handle regular messages
			wgMsg.Add(1)

			go func() {
				defer wgMsg.Done()
				logger.Debug().Msg("Telegram messenger started")

				if err := messenger.Telegram(ctx, eDB, ch, cfg.Telegram.Token, cfg.Telegram.ChatIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrTelegram, err)
					exitWithError.Store(true)
				}
			}()
		}

		// Slack sender
		if cfg.SlackEnabled {
			ch := make(chan interface{}) // broadcast listener
			defer close(ch)

			bcast.Register(ch) // broadcast registration
			defer bcast.Unregister(ch)

			// handle failed messages
			wgFailedMsg.Add(1)

			go func() {
				defer wgFailedMsg.Done()
				logger.Debug().Msg("Slack processing failed messages started")

				fetchAndSendFailedMsg(eDB, ch, messenger.SlackQueueName)
			}()

			// handle regular messages
			wgMsg.Add(1)

			go func() {
				defer wgMsg.Done()
				logger.Debug().Msg("Slack messenger started")

				if err := messenger.Slack(ctx, eDB, ch, cfg.Slack.Token, cfg.Slack.ChatIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrSlack, err)
					exitWithError.Store(true)
				}
			}()
		}

		// Mail Sender
		if cfg.MailEnabled {
			ch := make(chan interface{}) // broadcast listener
			defer close(ch)

			bcast.Register(ch) // broadcast registration
			defer bcast.Unregister(ch)

			// handle failed messages
			wgFailedMsg.Add(1)

			go func() {
				defer wgFailedMsg.Done()
				logger.Debug().Msg("Mail processing failed messages started")

				fetchAndSendFailedMsg(eDB, ch, messenger.MailQueueName)
			}()

			// handle regular messages
			wgMsg.Add(1)

			go func() {
				defer wgMsg.Done()
				logger.Debug().Msg("Mail messenger started")

				if err := messenger.Mail(ctx, eDB, ch, cfg.Mail.Server, cfg.Mail.Port, cfg.Mail.Username,
					cfg.Mail.Password, cfg.Mail.From, cfg.Mail.Subject, cfg.Mail.To, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrMail, err)
					exitWithError.Store(true)
				}
			}()
		}

		// Google Calendar Sender
		if cfg.CalendarEnabled {
			ch := make(chan interface{}) // broadcast listener
			defer close(ch)

			bcast.Register(ch) // broadcast registration
			defer bcast.Unregister(ch)

			// handle failed messages
			wgFailedMsg.Add(1)

			go func() {
				defer wgFailedMsg.Done()
				logger.Debug().Msg("Calendar processing failed messages started")

				fetchAndSendFailedMsg(eDB, ch, messenger.CalendarQueueName)
			}()

			// handle regular messages
			wgMsg.Add(1)

			go func() {
				defer wgMsg.Done()
				logger.Debug().Msg("Calendar messenger started")

				if err := messenger.Calendar(ctx, eDB, ch, cfg.Calendar.Name, *calTokFile, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrCalendar, err)
					exitWithError.Store(true)
				}
			}()
		}

		// WhatsApp sSender
		if cfg.WhatsAppEnabled {
			ch := make(chan interface{}) // broadcast listener
			defer close(ch)

			bcast.Register(ch) // broadcast registration
			defer bcast.Unregister(ch)

			// handle failed messages
			wgFailedMsg.Add(1)

			go func() {
				defer wgFailedMsg.Done()
				logger.Debug().Msg("WhatsApp processing failed messages started")

				fetchAndSendFailedMsg(eDB, ch, messenger.WhatsAppQueueName)
			}()

			// handle regular messages
			wgMsg.Add(1)

			go func() {
				defer wgMsg.Done()
				logger.Debug().Msg("WhatsApp messenger started")

				if err := messenger.WhatsApp(ctx, eDB, ch, cfg.WhatsApp.UserIDs, cfg.WhatsApp.Groups,
					*retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrWhatsApp, err)
					exitWithError.Store(true)
				}
			}()
		}

		// wait for all failed messages to be processed
		wgFailedMsg.Wait()

		// broadcast regular incoming messages
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

// msgDedup acts like a filter: processes all incoming messages, calls in to database checkWhatsAppConf and if it hasn't been found
// and if it is not an initial run, it will pass through to messengers for further alerting.
func msgDedup(ctx context.Context, eDB *db.Edb, wgFilter *sync.WaitGroup, gradesScraped <-chan msgtypes.Message, gradesMsg chan<- msgtypes.Message) {
	wgFilter.Add(1)

	go func() {
		defer wgFilter.Done()

		if !eDB.Existing() {
			logger.Info().Msg("Newly initialized database, won't sent alerts in this run")
		}

		// cache current time for later
		now := time.Now()

		for g := range gradesScraped {
			select {
			case <-ctx.Done():
				return
			default:
				// log all events
				if *debugEvents {
					logger.Debug().Msgf("Received event for: %v/%v: %+v", g.Username, g.Subject, g)
				}

				// checkWhatsAppConf if it is an already known alert
				found, err := eDB.CheckAndFlagTTL(g.Username, g.Subject, g.Fields)
				if err != nil {
					logger.Fatal().Msgf("Problem with database, cannot continue: %v", err)
				}

				// checkWhatsAppConf if is the initial run and send only if not
				if !found && eDB.Existing() {
					// checkWhatsAppConf if it is an old event that should be ignored
					if *relevancePeriod > 0 && !g.IsExam && len(g.Fields) > 0 {
						t, err := time.Parse(formatHRDateOnly, g.Fields[0])
						if err != nil {
							logger.Error().Msgf("Unable to parse date for: %v/%v: %+v: %v", g.Username, g.Subject, g, err)
						} else {
							// assume current or previous year
							if t.Month() > now.Month() {
								t = t.AddDate(now.Year()-1, 0, 0)
							} else {
								t = t.AddDate(now.Year(), 0, 0)
							}

							if time.Since(t) > *relevancePeriod {
								logger.Warn().Msgf("Ignoring changes in an old event: %v/%v: %+v", g.Username, g.Subject, g)

								continue
							}
						}
					}

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

// versionCheck checks for updates by comparing the current version of the
// application with the latest version available on GitHub. If a newer version
// is available, it logs an informational message. This function spawns a
// goroutine and uses a WaitGroup to synchronize with other goroutines.
func versionCheck(ctx context.Context, wgVersion *sync.WaitGroup) {
	wgVersion.Add(1)

	go func() {
		defer wgVersion.Done()

		// if we don't have a tag or if it is a local source-build, we don't need to checkWhatsAppConf for updates
		if GitTag == "" || GitDirty != "" {
			return
		}

		var currentTag, latestTag semver.Version

		var err error

		// semver-parse current version
		if GitTag[0] == 'v' {
			currentTag, err = semver.Parse(GitTag[1:])
		} else {
			currentTag, err = semver.Parse(GitTag)
		}

		if err != nil {
			logger.Error().Msgf("Unable to parse current version of e-dnevnik-bot: %v", err)

			return
		}

		client := github.NewClient(nil)

		// get latest release from GitHub
		latestRelease, _, err := client.Repositories.GetLatestRelease(ctx, githubOrg, githubRepo)
		if err != nil {
			logger.Error().Msgf("Unable to checkWhatsAppConf latest version of e-dnevnik-bot: %v", err)

			return
		}

		// semver-parse latest version
		tagName := *latestRelease.TagName
		if tagName[0] == 'v' {
			latestTag, err = semver.Parse(tagName[1:])
		} else {
			latestTag, err = semver.Parse(tagName)
		}

		if err != nil {
			logger.Error().Msgf("Unable to parse latest version of e-dnevnik-bot: %v", err)

			return
		}

		// alert if there is a newer version
		if latestTag.Compare(currentTag) == 1 {
			logger.Info().Msgf("Newer version of e-dnevnik-bot is available: %v", latestTag)
		}
	}()
}
