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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/dkorunic/e-dnevnik-bot/config"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/messenger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/scrape"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
	"github.com/google/go-github/v86/github"
	"github.com/teivah/broadcast"
	"github.com/tj/go-spin"
	"golang.org/x/oauth2"
)

const (
	broadcastBufLen    = 100                    // events to broadcast for sending at once
	spinnerRotateDelay = 100 * time.Millisecond // spinner delay
	githubOrg          = "dkorunic"
	githubRepo         = "e-dnevnik-bot"
	// versionCheckTimeout bounds the entire GitHub release-check in one poll
	// cycle so a stalled GitHub API does not leak the version-check goroutine
	// past its poll interval.
	versionCheckTimeout = 30 * time.Second
)

var (
	ErrScrapingUser = errors.New("error scraping data for User")
	ErrDiscord      = errors.New("Discord messenger issue")  //nolint:staticcheck
	ErrTelegram     = errors.New("Telegram messenger issue") //nolint:staticcheck
	ErrSlack        = errors.New("Slack messenger issue")    //nolint:staticcheck
	ErrMail         = errors.New("Mail messenger issue")     //nolint:staticcheck
	ErrCalendar     = errors.New("Google Calendar issue")    //nolint:staticcheck
	ErrWhatsApp     = errors.New("WhatsApp issue")           //nolint:staticcheck

	// formatHRDateOnly matches the "D.M." (day.month.) format used in the
	// portal's grade date column. The digits are Go's magic reference-time
	// tokens: 2 = day-of-month, 1 = month; the two literal '.' characters are
	// required separators. Do NOT "normalise" this to "2006-01-02"-style — it
	// would break parsing of real portal values like "15.4.".
	formatHRDateOnly = "2.1."
)

// scrapers will call subjects/grades/exams scraping for every configured AAI/AOSI User and send grades/exams messages
// to a channel.
func scrapers(ctx context.Context, wgScrape *sync.WaitGroup, gradesScraped chan<- msgtypes.Message, cfg config.TomlConfig) {
	logger.Debug().Msg("Starting scrapers")

	for _, i := range cfg.User {
		wgScrape.Go(func() {
			err := scrape.GetGradesAndEvents(ctx, gradesScraped, i.Username, i.Password, *retries)
			if err != nil {
				logger.Warn().Msgf("%v %v: %v", ErrScrapingUser, i.Username, err)
				exitWithError.Store(true)
			}
		})
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
func msgSend(ctx context.Context, eDB *sqlitedb.Edb, wgMsg *sync.WaitGroup, gradesMsg <-chan msgtypes.Message, cfg config.TomlConfig) {
	wgMsg.Go(func() {
		relay := broadcast.NewRelay[msgtypes.Message]()

		// Close relay first, then wait — reversed order deadlocks listener loops.
		var wgInner sync.WaitGroup

		defer func() {
			relay.Close()
			wgInner.Wait()
		}()

		if cfg.DiscordEnabled {
			l := relay.Listener(broadcastBufLen)

			wgInner.Add(1)

			wgMsg.Go(func() {
				defer wgInner.Done()

				if err := messenger.Discord(ctx, eDB, l.Ch(), cfg.Discord.Token, cfg.Discord.UserIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrDiscord, err)
					exitWithError.Store(true)
				}
			})
		}

		if cfg.TelegramEnabled {
			l := relay.Listener(broadcastBufLen)

			wgInner.Add(1)

			wgMsg.Go(func() {
				defer wgInner.Done()

				if err := messenger.Telegram(ctx, eDB, l.Ch(), cfg.Telegram.Token, cfg.Telegram.ChatIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrTelegram, err)
					exitWithError.Store(true)
				}
			})
		}

		if cfg.SlackEnabled {
			l := relay.Listener(broadcastBufLen)

			wgInner.Add(1)

			wgMsg.Go(func() {
				defer wgInner.Done()

				if err := messenger.Slack(ctx, eDB, l.Ch(), cfg.Slack.Token, cfg.Slack.ChatIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrSlack, err)
					exitWithError.Store(true)
				}
			})
		}

		if cfg.MailEnabled {
			l := relay.Listener(broadcastBufLen)

			wgInner.Add(1)

			wgMsg.Go(func() {
				defer wgInner.Done()

				if err := messenger.Mail(ctx, eDB, l.Ch(), cfg.Mail.Server, cfg.Mail.Port, cfg.Mail.Username,
					cfg.Mail.Password, cfg.Mail.From, cfg.Mail.Subject, cfg.Mail.To, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrMail, err)
					exitWithError.Store(true)
				}
			})
		}

		if cfg.CalendarEnabled {
			l := relay.Listener(broadcastBufLen)

			wgInner.Add(1)

			wgMsg.Go(func() {
				defer wgInner.Done()

				if err := messenger.Calendar(ctx, eDB, l.Ch(), cfg.Calendar.Name, *calTokFile, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrCalendar, err)
					exitWithError.Store(true)
				}
			})
		}

		if cfg.WhatsAppEnabled {
			l := relay.Listener(broadcastBufLen)

			wgInner.Add(1)

			wgMsg.Go(func() {
				defer wgInner.Done()

				if err := messenger.WhatsApp(ctx, eDB, l.Ch(), cfg.WhatsApp.UserIDs, cfg.WhatsApp.Groups,
					*retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrWhatsApp, err)
					exitWithError.Store(true)
				}
			})
		}

		for g := range gradesMsg {
			select {
			case <-ctx.Done():
				return
			default:
				relay.NotifyCtx(ctx, g)
			}
		}
	})
}

// msgDedup acts like a filter: processes all incoming messages, calls in to database check and if it hasn't been found
// and if it is not an initial run, it will pass through to messengers for further alerting.
func msgDedup(ctx context.Context, eDB *sqlitedb.Edb, wgFilter *sync.WaitGroup, gradesScraped <-chan msgtypes.Message, gradesMsg chan<- msgtypes.Message) {
	wgFilter.Go(func() {
		// Close gradesMsg on exit so msgSend's broadcast loop unblocks.
		defer close(gradesMsg)

		if !eDB.Existing() {
			logger.Info().Msg("Newly initialized database, won't sent alerts in this run")
		}

		now := time.Now()

		for g := range gradesScraped {
			select {
			case <-ctx.Done():
				return
			default:
				if *debugEvents {
					logger.Debug().Msgf("Received event for: %v/%v: %+v", g.Username, g.Subject, g)
				}

				if !*readingList && g.Code == msgtypes.Reading {
					continue
				}

				found, err := eDB.CheckAndFlagTTL(ctx, g.Username, g.Subject, g.Fields)
				if err != nil {
					logger.Fatal().Msgf("Problem with database, cannot continue: %v", err)
				}

				// Skip on first run or duplicate: prevents first-install flood / repeat alerts.
				//nolint:nestif
				if !found && eDB.Existing() {
					if *relevancePeriod > 0 && g.Code == msgtypes.Exam && !g.Timestamp.IsZero() {
						if time.Since(g.Timestamp) > *relevancePeriod {
							logger.Warn().Msgf("Ignoring old exam event: %v/%v: %+v", g.Username, g.Subject, g)

							continue
						}
					}

					if *relevancePeriod > 0 && g.Code == msgtypes.Grade && len(g.Fields) > 0 {
						// XXX Fields[0] assumed to be the grade date.
						t, err := time.Parse(formatHRDateOnly, g.Fields[0])
						if err != nil {
							// Fail-open: prefer stale alert to silent drop.
							logger.Error().Msgf("Unable to parse date for: %v/%v: %+v: %v", g.Username, g.Subject, g, err)
						} else {
							// Future day.month. implies previous year.
							if t.Month() > now.Month() || (t.Month() == now.Month() && t.Day() > now.Day()) {
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

					select {
					case gradesMsg <- g:
					case <-ctx.Done():
						return
					}
				}
			}
		}
	})
}

// spinner shows a spiffy terminal spinner until done is closed.
func spinner(done <-chan struct{}) {
	s := spin.New()

	for {
		fmt.Printf("\rWaiting... %v", s.Next())

		// Cancellable wait so shutdown isn't held by an in-flight Sleep.
		select {
		case <-done:
			fmt.Print("\r")

			return
		case <-time.After(spinnerRotateDelay):
		}
	}
}

// versionCheck checks for newer versions of e-dnevnik-bot on GitHub.
// It uses the git tag information to compare the current version with the latest version.
// If a newer version is available, it prints a message indicating how many releases are behind.
// NOTE: This function is only run if the program is not running from a local source-build (i.e. if GitTag is not empty and GitDirty is empty).
// It does not check for updates if the program is running from a local source-build, as the user is expected to be aware of the latest version.
func versionCheck(ctx context.Context, wgVersion *sync.WaitGroup) {
	wgVersion.Go(func() {
		// Skip local source-builds — user owns their own version.
		if GitTag == "" || GitDirty != "" {
			return
		}

		currentTag, err := semver.NewVersion(strings.TrimPrefix(GitTag, "v"))
		if err != nil || currentTag == nil {
			logger.Error().Msgf("Unable to parse current version of e-dnevnik-bot: %v", err)

			return
		}

		// Bounded timeout so a stalled GitHub API can't outlive the poll cycle.
		vctx, cancel := context.WithTimeout(ctx, versionCheckTimeout)
		defer cancel()

		client := githubClient(vctx)

		latestRelease, _, err := client.Repositories.GetLatestRelease(vctx, githubOrg, githubRepo)
		if err != nil || latestRelease == nil {
			logger.Error().Msgf("Unable to check for latest release of e-dnevnik-bot: %v", err)

			return
		}

		if latestRelease.TagName == nil {
			logger.Error().Msg("Unable to parse latest release of e-dnevnik-bot: nil TagName")

			return
		}

		latestTag, err := semver.NewVersion(strings.TrimPrefix(*latestRelease.TagName, "v"))
		if err != nil || latestTag == nil {
			logger.Error().Msgf("Unable to parse latest release of e-dnevnik-bot: %v", err)

			return
		}

		if latestTag.Compare(currentTag) == 1 {
			logger.Info().Msgf("Newer version of e-dnevnik-bot is available: %v (you are on %v)", latestTag, currentTag)
		}
	})
}

// githubClient returns a new GitHub client for the given context.
//
// If the GITHUB_TOKEN environment variable is not set, the client will be created without authentication.
//
// Parameters:
// - ctx: the context.Context for the HTTP client.
//
// Returns:
// - *github.Client: the GitHub client.
func githubClient(ctx context.Context) *github.Client {
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		return github.NewClient(nil)
	}

	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: token},
	)
	tc := oauth2.NewClient(ctx, ts)

	return github.NewClient(tc)
}
