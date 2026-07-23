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
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/scrape"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/google/go-github/v89/github"
	"github.com/tj/go-spin"
)

const (
	messengerBufLen    = 100                    // per-messenger fan-out channel buffer
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
				// Shutdown-induced cancellation is not a cycle failure.
				if ctx.Err() != nil && errors.Is(err, context.Canceled) {
					logger.Debug().Msgf("Scraping aborted by shutdown for user %v", i.Username)

					return
				}

				logger.Warn().Msgf("%v %v: %v", ErrScrapingUser, i.Username, err)
				exitWithError.Store(true)
			}
		})
	}
}

// flagMessengerError logs a messenger failure and latches the run as failed —
// unless the failure is a shutdown-induced cancellation, which is part of a
// normal stop and must not turn a clean SIGTERM into a non-zero exit.
func flagMessengerError(ctx context.Context, sentinel, err error) {
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		logger.Debug().Msgf("Messenger aborted by shutdown: %v", err)

		return
	}

	logger.Warn().Msgf("%v: %v", sentinel, err)
	exitWithError.Store(true)
}

// msgSend fans messages from gradesMsg out to every enabled messenger, each
// draining its own buffered channel in its own goroutine.
//
// The fan-out is non-blocking: a messenger whose buffer is full (fallen behind,
// e.g. mail mid-retry) has the message spilled to its queue for a later cycle
// instead of blocking the others — isolating a slow messenger's failure domain.
//
// Two-level WaitGroup: the deferred sequence closes every messenger channel
// *then* wgInner.Wait(). Reversed order deadlocks — a drain loop exits only
// once its channel closes.
func msgSend(ctx context.Context, eDB *sqlitedb.Edb, wgMsg *sync.WaitGroup, gradesMsg <-chan msgtypes.Message, cfg config.TomlConfig) {
	wgMsg.Go(func() {
		var wgInner sync.WaitGroup

		// sink pairs a messenger's channel with its queue for full-buffer spills.
		type sink struct {
			ch    chan msgtypes.Message
			queue []byte
		}

		var sinks []sink

		// start registers a messenger's buffered channel as a sink and drains it
		// in a tracked goroutine.
		start := func(queueName []byte, run func(ch <-chan msgtypes.Message)) {
			ch := make(chan msgtypes.Message, messengerBufLen)
			sinks = append(sinks, sink{ch: ch, queue: queueName})

			wgInner.Add(1)

			wgMsg.Go(func() {
				defer wgInner.Done()

				run(ch)
			})
		}

		// Close before wait (see doc): drain loops exit only on channel close.
		defer func() {
			for _, s := range sinks {
				close(s.ch)
			}

			wgInner.Wait()
		}()

		if cfg.DiscordEnabled {
			start(messenger.DiscordQueueName, func(ch <-chan msgtypes.Message) {
				if err := messenger.Discord(ctx, eDB, ch, messenger.DiscordConfig{
					Token:   cfg.Discord.Token,
					UserIDs: cfg.Discord.UserIDs,
					Retries: *retries,
				}); err != nil {
					flagMessengerError(ctx, ErrDiscord, err)
				}
			})
		}

		if cfg.TelegramEnabled {
			start(messenger.TelegramQueueName, func(ch <-chan msgtypes.Message) {
				if err := messenger.Telegram(ctx, eDB, ch, messenger.TelegramConfig{
					Token:   cfg.Telegram.Token,
					ChatIDs: cfg.Telegram.ChatIDs,
					Retries: *retries,
				}); err != nil {
					flagMessengerError(ctx, ErrTelegram, err)
				}
			})
		}

		if cfg.SlackEnabled {
			start(messenger.SlackQueueName, func(ch <-chan msgtypes.Message) {
				if err := messenger.Slack(ctx, eDB, ch, messenger.SlackConfig{
					Token:   cfg.Slack.Token,
					ChatIDs: cfg.Slack.ChatIDs,
					Retries: *retries,
				}); err != nil {
					flagMessengerError(ctx, ErrSlack, err)
				}
			})
		}

		if cfg.MailEnabled {
			start(messenger.MailQueueName, func(ch <-chan msgtypes.Message) {
				if err := messenger.Mail(ctx, eDB, ch, messenger.MailConfig{
					Server:   cfg.Mail.Server,
					Port:     cfg.Mail.Port,
					Username: cfg.Mail.Username,
					Password: cfg.Mail.Password,
					From:     cfg.Mail.From,
					Subject:  cfg.Mail.Subject,
					To:       cfg.Mail.To,
					Retries:  *retries,
				}); err != nil {
					flagMessengerError(ctx, ErrMail, err)
				}
			})
		}

		if cfg.CalendarEnabled {
			start(messenger.CalendarQueueName, func(ch <-chan msgtypes.Message) {
				if err := messenger.Calendar(ctx, eDB, ch, messenger.CalendarConfig{
					Name:    cfg.Calendar.Name,
					TokFile: *calTokFile,
					Retries: *retries,
				}); err != nil {
					flagMessengerError(ctx, ErrCalendar, err)
				}
			})
		}

		// Calendar configured but not yet initializable: queue-only stub
		// preserves exams. Mutually exclusive with CalendarEnabled.
		if cfg.CalendarDeferred {
			start(messenger.CalendarQueueName, func(ch <-chan msgtypes.Message) {
				messenger.CalendarDeferred(ctx, eDB, ch)
			})
		}

		if cfg.WhatsAppEnabled {
			start(messenger.WhatsAppQueueName, func(ch <-chan msgtypes.Message) {
				if err := messenger.WhatsApp(ctx, eDB, ch, messenger.WhatsAppConfig{
					UserIDs: cfg.WhatsApp.UserIDs,
					Groups:  cfg.WhatsApp.Groups,
					Retries: *retries,
				}); err != nil {
					flagMessengerError(ctx, ErrWhatsApp, err)
				}
			})
		}

		// Non-blocking: a full (behind) buffer spills to the queue, never blocks.
		// gradesMsg close ends this on shutdown.
		for g := range gradesMsg {
			for _, s := range sinks {
				select {
				case s.ch <- g:
				default:
					storeOverflow(ctx, eDB, s.queue, g)
				}
			}
		}
	})
}

// overflowStoreTimeout bounds the detached spill-to-queue write.
const overflowStoreTimeout = 5 * time.Second

// storeOverflow spills g to a messenger's queue when its channel is full,
// detached from ctx so a spill during shutdown still lands. A store failure
// only logs — the event is already dedup-flagged, so there is no fallback.
func storeOverflow(ctx context.Context, eDB *sqlitedb.Edb, queueName []byte, g msgtypes.Message) {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), overflowStoreTimeout)
	defer cancel()

	if err := queue.StoreFailedMsgs(sctx, eDB, queueName, g); err != nil {
		logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
	}
}

// msgDedup acts like a filter: processes all incoming messages, calls in to database check and if it hasn't been found
// and if it is not an initial run, it will pass through to messengers for further alerting.
func msgDedup(ctx context.Context, eDB *sqlitedb.Edb, wgFilter *sync.WaitGroup, gradesScraped <-chan msgtypes.Message, gradesMsg chan<- msgtypes.Message) {
	wgFilter.Go(func() {
		// Close gradesMsg on exit so msgSend's fan-out loop unblocks.
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

			found, err := eDB.CheckAndFlagTTL(ctx, g.Username, g.Subject, g.Fields)
			if err != nil {
				// Not Fatal: os.Exit here would bypass in-flight messenger
				// queue writes and deferred cleanup. SIGTERM-to-self runs the
				// normal graceful shutdown instead; unforwarded events are
				// unflagged and re-scrape next run.
				logger.Error().Msgf("Problem with database, cannot continue: %v", err)
				exitWithError.Store(true)
				messenger.RequestShutdown()

				return
			}

			// Skip on first run or duplicate: prevents first-install flood / repeat alerts.
			if found || !eDB.Existing() {
				continue
			}

			// Seeded above but only forwarded when --readinglist is set. Flagging
			// regardless (not skipping before CheckAndFlagTTL) prevents a flood
			// when the flag is first enabled.
			if !*readingList && g.Code == msgtypes.Reading {
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

// spinner shows a spiffy terminal spinner until done is closed. It writes to
// stderr so the stdout stream stays parseable when logs are JSON.
func spinner(done <-chan struct{}) {
	s := spin.New()

	for {
		fmt.Fprintf(os.Stderr, "\rWaiting... %v", s.Next())

		// Cancellable wait so shutdown isn't held by an in-flight Sleep.
		select {
		case <-done:
			fmt.Fprint(os.Stderr, "\r")

			return
		case <-time.After(spinnerRotateDelay):
		}
	}
}

// versionCheck logs a notice if a newer release exists on GitHub. Skipped for
// local/dirty source builds; the GitHub call is bounded by versionCheckTimeout.
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
			// Shutdown cancelling vctx mid-request is not an app error.
			if ctx.Err() == nil {
				logger.Error().Msgf("Unable to check for latest release of e-dnevnik-bot: %v", err)
			}

			return
		}

		if latestRelease.TagName == "" {
			logger.Error().Msg("Unable to parse latest release of e-dnevnik-bot: empty TagName")

			return
		}

		latestTag, err := semver.NewVersion(strings.TrimPrefix(latestRelease.TagName, "v"))
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
