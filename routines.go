// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

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
	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/messenger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/scrape"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/google/go-github/v88/github"
	"github.com/teivah/broadcast"
	"github.com/tj/go-spin"
)

const (
	broadcastBufLen    = 100                    // events to broadcast for sending at once
	spinnerRotateDelay = 100 * time.Millisecond // spinner delay
	githubOrg          = "dkorunic"
	githubRepo         = "e-dnevnik-bot"
	// versionCheckTimeout bounds the GitHub release-check so a stalled API can't outlive one poll cycle.
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

	// formatHRDateOnly parses the portal's "D.M." grade date column.
	// Do not normalise: portal values like "15.4." would stop parsing.
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
				// Close on early exit so the broadcast loop can't wedge on an undrained listener.
				defer l.Close()

				if err := messenger.Discord(ctx, eDB, l.Ch(), messenger.DiscordConfig{
					Token:   cfg.Discord.Token,
					UserIDs: cfg.Discord.UserIDs,
					Retries: *retries,
				}); err != nil {
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
				defer l.Close()

				if err := messenger.Telegram(ctx, eDB, l.Ch(), messenger.TelegramConfig{
					Token:   cfg.Telegram.Token,
					ChatIDs: cfg.Telegram.ChatIDs,
					Retries: *retries,
				}); err != nil {
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
				defer l.Close()

				if err := messenger.Slack(ctx, eDB, l.Ch(), messenger.SlackConfig{
					Token:   cfg.Slack.Token,
					ChatIDs: cfg.Slack.ChatIDs,
					Retries: *retries,
				}); err != nil {
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
				defer l.Close()

				if err := messenger.Mail(ctx, eDB, l.Ch(), messenger.MailConfig{
					Server:   cfg.Mail.Server,
					Port:     cfg.Mail.Port,
					Username: cfg.Mail.Username,
					Password: cfg.Mail.Password,
					From:     cfg.Mail.From,
					Subject:  cfg.Mail.Subject,
					To:       cfg.Mail.To,
					Retries:  *retries,
				}); err != nil {
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
				defer l.Close()

				if err := messenger.Calendar(ctx, eDB, l.Ch(), messenger.CalendarConfig{
					Name:    cfg.Calendar.Name,
					TokFile: *calTokFile,
					Retries: *retries,
				}); err != nil {
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
				defer l.Close()

				if err := messenger.WhatsApp(ctx, eDB, l.Ch(), messenger.WhatsAppConfig{
					UserIDs: cfg.WhatsApp.UserIDs,
					Groups:  cfg.WhatsApp.Groups,
					Retries: *retries,
				}); err != nil {
					logger.Warn().Msgf("%v: %v", ErrWhatsApp, err)
					exitWithError.Store(true)
				}
			})
		}

		// Lossless blocking fan-out; gradesMsg close terminates this on shutdown.
		for g := range gradesMsg {
			relay.Notify(g)
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
			logger.Info().Msg("Newly initialized database, won't send alerts in this run")
		}

		now := time.Now()

		for g := range gradesScraped {
			// Bail before flagging: unflagged events re-scrape next run; flagged ones can't drop.
			if ctx.Err() != nil {
				return
			}

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
			if found || !eDB.Existing() {
				continue
			}

			if isStaleEvent(g, now) {
				continue
			}

			logger.Info().Msgf("New alert for: %v/%v: %+v", g.Username, g.Subject, g)

			// Blocking handoff: flagged events must reach msgSend; receiver lives until close.
			gradesMsg <- g
		}
	})
}

// isStaleEvent reports whether g falls outside the configured relevance window
// and should be suppressed. Only Exam and Grade events are time-filtered; all
// other codes (and a zero relevancePeriod) are always treated as fresh. A grade
// date that fails to parse fails open — a stale alert is preferred to a silent
// drop. The matching log line is emitted here so the caller stays a flat guard.
func isStaleEvent(g msgtypes.Message, now time.Time) bool {
	if *relevancePeriod <= 0 {
		return false
	}

	switch {
	case g.Code == msgtypes.Exam && !g.Timestamp.IsZero():
		if time.Since(g.Timestamp) > *relevancePeriod {
			logger.Warn().Msgf("Ignoring old exam event: %v/%v: %+v", g.Username, g.Subject, g)

			return true
		}
	case g.Code == msgtypes.Grade && len(g.Fields) > 0:
		// XXX Fields[0] assumed to be the grade date.
		t, err := time.Parse(formatHRDateOnly, g.Fields[0])
		if err != nil {
			// Fail-open: prefer stale alert to silent drop.
			logger.Error().Msgf("Unable to parse date for: %v/%v: %+v: %v", g.Username, g.Subject, g, err)

			return false
		}

		// Future day.month. implies previous year.
		if t.Month() > now.Month() || (t.Month() == now.Month() && t.Day() > now.Day()) {
			t = t.AddDate(now.Year()-1, 0, 0)
		} else {
			t = t.AddDate(now.Year(), 0, 0)
		}

		if time.Since(t) > *relevancePeriod {
			logger.Warn().Msgf("Ignoring changes in an old event: %v/%v: %+v", g.Username, g.Subject, g)

			return true
		}
	}

	return false
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

		client, err := githubClient()
		if err != nil {
			logger.Error().Msgf("Unable to create GitHub client: %v", err)

			return
		}

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

// githubClient returns a new GitHub client, authenticated via GITHUB_TOKEN when set.
func githubClient() (*github.Client, error) {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return github.NewClient(github.WithAuthToken(token))
	}

	return github.NewClient()
}
