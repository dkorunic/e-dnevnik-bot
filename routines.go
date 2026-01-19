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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/dkorunic/e-dnevnik-bot/config"
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/messenger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/scrape"
	"github.com/google/go-github/v81/github"
	"github.com/teivah/broadcast"
	"github.com/tj/go-spin"
	"golang.org/x/oauth2"
)

const (
	broadcastBufLen    = 10                     // events to broadcast for sending at once
	spinnerRotateDelay = 100 * time.Millisecond // spinner delay
	githubOrg          = "dkorunic"
	githubRepo         = "e-dnevnik-bot"
)

var (
	ErrScrapingUser    = errors.New("error scraping data for User")
	ErrDiscord         = errors.New("Discord messenger issue")  //nolint:staticcheck
	ErrTelegram        = errors.New("Telegram messenger issue") //nolint:staticcheck
	ErrSlack           = errors.New("Slack messenger issue")    //nolint:staticcheck
	ErrMail            = errors.New("Mail messenger issue")     //nolint:staticcheck
	ErrCalendar        = errors.New("Google Calendar issue")    //nolint:staticcheck
	ErrWhatsApp        = errors.New("WhatsApp issue")           //nolint:staticcheck
	ErrNoValidReleases = errors.New("no valid SemVer releases found")

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
func msgSend(ctx context.Context, eDB *db.Edb, wgMsg *sync.WaitGroup, gradesMsg <-chan msgtypes.Message, cfg config.TomlConfig) {
	wgMsg.Go(func() {
		relay := broadcast.NewRelay[msgtypes.Message]()
		defer relay.Close()

		// Discord sender
		if cfg.DiscordEnabled {
			l := relay.Listener(broadcastBufLen)

			wgMsg.Go(func() {
				if err := messenger.Discord(ctx, eDB, l.Ch(), cfg.Discord.Token, cfg.Discord.UserIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrDiscord, err)
					exitWithError.Store(true)
				}
			})
		}

		// Telegram sender
		if cfg.TelegramEnabled {
			l := relay.Listener(broadcastBufLen)

			wgMsg.Go(func() {
				if err := messenger.Telegram(ctx, eDB, l.Ch(), cfg.Telegram.Token, cfg.Telegram.ChatIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrTelegram, err)
					exitWithError.Store(true)
				}
			})
		}

		// Slack sender
		if cfg.SlackEnabled {
			l := relay.Listener(broadcastBufLen)

			wgMsg.Go(func() {
				if err := messenger.Slack(ctx, eDB, l.Ch(), cfg.Slack.Token, cfg.Slack.ChatIDs, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrSlack, err)
					exitWithError.Store(true)
				}
			})
		}

		// Mail Sender
		if cfg.MailEnabled {
			l := relay.Listener(broadcastBufLen)

			wgMsg.Go(func() {
				if err := messenger.Mail(ctx, eDB, l.Ch(), cfg.Mail.Server, cfg.Mail.Port, cfg.Mail.Username,
					cfg.Mail.Password, cfg.Mail.From, cfg.Mail.Subject, cfg.Mail.To, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrMail, err)
					exitWithError.Store(true)
				}
			})
		}

		// Google Calendar Sender
		if cfg.CalendarEnabled {
			l := relay.Listener(broadcastBufLen)

			wgMsg.Go(func() {
				if err := messenger.Calendar(ctx, eDB, l.Ch(), cfg.Calendar.Name, *calTokFile, *retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrCalendar, err)
					exitWithError.Store(true)
				}
			})
		}

		// WhatsApp sSender
		if cfg.WhatsAppEnabled {
			l := relay.Listener(broadcastBufLen)

			wgMsg.Go(func() {
				if err := messenger.WhatsApp(ctx, eDB, l.Ch(), cfg.WhatsApp.UserIDs, cfg.WhatsApp.Groups,
					*retries); err != nil {
					logger.Warn().Msgf("%v: %v", ErrWhatsApp, err)
					exitWithError.Store(true)
				}
			})
		}

		// broadcast incoming messages with guaranteed delivery and context
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
func msgDedup(ctx context.Context, eDB *db.Edb, wgFilter *sync.WaitGroup, gradesScraped <-chan msgtypes.Message, gradesMsg chan<- msgtypes.Message) {
	wgFilter.Go(func() {
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

				// skip reading list events
				if !*readingList && g.Code == msgtypes.Reading {
					continue
				}

				// check if it is an already known alert
				found, err := eDB.CheckAndFlagTTL(g.Username, g.Subject, g.Fields)
				if err != nil {
					logger.Fatal().Msgf("Problem with database, cannot continue: %v", err)
				}

				// check if is the initial run and send only if not
				//nolint:nestif
				if !found && eDB.Existing() {
					// check if it is an old grade edit that should be ignored
					if *relevancePeriod > 0 && g.Code == msgtypes.Grade && len(g.Fields) > 0 {
						// XXX hardcoded location of the date for grades
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
	})
}

// spinner shows a spiffy terminal spinner while waiting endlessly.
func spinner() {
	s := spin.New()

	for {
		fmt.Printf("\rWaiting... %v", s.Next())
		time.Sleep(spinnerRotateDelay)
	}
}

// versionCheck checks for newer versions of e-dnevnik-bot on GitHub.
// It uses the git tag information to compare the current version with the latest version.
// If a newer version is available, it prints a message indicating how many releases are behind.
// NOTE: This function is only run if the program is not running from a local source-build (i.e. if GitTag is not empty and GitDirty is empty).
// It does not check for updates if the program is running from a local source-build, as the user is expected to be aware of the latest version.
func versionCheck(ctx context.Context, wgVersion *sync.WaitGroup) {
	wgVersion.Go(func() {
		// if we don't have a tag or if it is a local source-build, we don't need to check for updates
		if GitTag == "" || GitDirty != "" {
			return
		}

		// semver-parse current version
		currentTag, err := semver.NewVersion(strings.TrimPrefix(GitTag, "v"))
		if err != nil || currentTag == nil {
			logger.Error().Msgf("Unable to parse current version of e-dnevnik-bot: %v", err)

			return
		}

		client := githubClient(ctx)

		// get latest release from GitHub
		latestRelease, _, err := client.Repositories.GetLatestRelease(ctx, githubOrg, githubRepo)
		if err != nil || latestRelease == nil {
			logger.Error().Msgf("Unable to check for latest release of e-dnevnik-bot: %v", err)

			return
		}

		// semver-parse latest version
		latestTag, err := semver.NewVersion(strings.TrimPrefix(*latestRelease.TagName, "v"))
		if err != nil || latestTag == nil {
			logger.Error().Msgf("Unable to parse latest release of e-dnevnik-bot: %v", err)

			return
		}

		// alert if there is a newer version
		if latestTag.Compare(currentTag) == 1 {
			releasedVersions, err := fetchReleasedVersions(ctx, client, githubOrg, githubRepo)
			if err != nil {
				logger.Error().Msgf("Failed to fetch releases of e-dnevnik-bot: %v", err)

				return
			}

			sort.Sort(semver.Collection(releasedVersions))
			behind := countNewerVersions(currentTag, releasedVersions)

			logger.Info().Msgf("Newer version of e-dnevnik-bot is available: %v, you are %v releases behind", latestTag, behind)
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

// fetchReleasedVersions fetches all released versions of a given GitHub repository and returns them as a slice of *semver.Version.
//
// It takes the following parameters:
// - ctx: the context.Context for the HTTP client.
// - client: the *github.Client for the GitHub API.
// - owner: the string representing the owner of the repository.
// - repo: the string representing the name of the repository.
//
// The function returns the following:
// - []*semver.Version: a slice of *semver.Version representing all released versions of the repository.
// - error: an error if any occurred during the execution of the function.
func fetchReleasedVersions(ctx context.Context, client *github.Client, owner, repo string) ([]*semver.Version, error) {
	var versions []*semver.Version

	opt := &github.ListOptions{PerPage: 100}

	for {
		releases, resp, err := client.Repositories.ListReleases(ctx, owner, repo, opt)
		if err != nil {
			return nil, err
		}

		for _, r := range releases {
			if r.TagName == nil {
				continue
			}

			v, err := semver.NewVersion(strings.TrimPrefix(*r.TagName, "v"))
			if err != nil {
				continue
			}

			versions = append(versions, v)
		}

		if resp.NextPage == 0 {
			break
		}

		opt.Page = resp.NextPage
	}

	if len(versions) == 0 {
		return nil, ErrNoValidReleases
	}

	return versions, nil
}

// countNewerVersions counts the number of versions in the versions slice that are newer than the current version.
//
// It takes the following parameters:
// - current: the current version to compare with.
// - versions: a slice of versions to count.
//
// It returns the number of versions that are newer than the current version.
func countNewerVersions(current *semver.Version, versions []*semver.Version) int {
	count := 0

	for _, v := range versions {
		if v.GreaterThan(current) {
			count++
		}
	}

	return count
}
