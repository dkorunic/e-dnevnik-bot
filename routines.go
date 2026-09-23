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
	"github.com/google/go-github/v92/github"
	"github.com/tj/go-spin"
)

const (
	messengerBufLen    = 100                    // per-messenger fan-out channel buffer
	spinnerRotateDelay = 100 * time.Millisecond // spinner delay
	githubOrg          = "dkorunic"
	githubRepo         = "e-dnevnik-bot"
	// Bounds the release check so a stalled API can't outlive a poll cycle.
	versionCheckTimeout = 30 * time.Second
)

var (
	ErrScrapingUser = errors.New("error scraping data for User")
	ErrFanOutPanic  = errors.New("message fan-out panicked")
	ErrDedupPanic   = errors.New("message dedup panicked")
	ErrDiscord      = errors.New("Discord messenger issue")  //nolint:staticcheck
	ErrTelegram     = errors.New("Telegram messenger issue") //nolint:staticcheck
	ErrSlack        = errors.New("Slack messenger issue")    //nolint:staticcheck
	ErrMail         = errors.New("Mail messenger issue")     //nolint:staticcheck
	ErrCalendar     = errors.New("Google Calendar issue")    //nolint:staticcheck
	ErrCalDAV       = errors.New("CalDAV issue")             //nolint:staticcheck
	ErrWhatsApp     = errors.New("WhatsApp issue")           //nolint:staticcheck

	// Parses the portal's "D.M." grade date column. Do not normalise —
	// values like "15.4." would stop parsing.
	formatHRDateOnly = "2.1."
)

// scrapeStage is a test seam: runPollCycle's teardown ordering is observable
// only with events in flight, which otherwise needs the live portal.
var scrapeStage = scrapers

// scrapers scrapes grades and exams for every configured AAI/AOSI user.
func scrapers(ctx context.Context, wgScrape *sync.WaitGroup, gradesScraped chan<- msgtypes.Message, cfg config.TomlConfig) {
	logger.Debug().Msg("Starting scrapers")

	for _, i := range cfg.User {
		wgScrape.Go(func() {
			err := scrape.GetGradesAndEvents(ctx, gradesScraped, i.Username, i.Password, *retries)
			if err != nil {
				// A shutdown is not a cycle failure.
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

// flagMessengerError latches the run as failed, except on shutdown
// cancellation — a clean SIGTERM must not exit non-zero.
func flagMessengerError(ctx context.Context, sentinel, err error) {
	if ctx.Err() != nil && errors.Is(err, context.Canceled) {
		logger.Debug().Msgf("Messenger aborted by shutdown: %v", err)

		return
	}

	logger.Warn().Msgf("%v: %v", sentinel, err)
	exitWithError.Store(true)
}

// msgSend fans gradesMsg out to every enabled messenger, one buffered channel
// and goroutine each. Delivery is non-blocking; see dispatch.
//
// The deferred close must precede wgInner.Wait(): a drain loop exits only once
// its channel closes, so the reverse order deadlocks.
func msgSend(ctx context.Context, eDB *sqlitedb.Edb, wgMsg *sync.WaitGroup, gradesMsg <-chan msgtypes.Message, cfg config.TomlConfig) {
	wgMsg.Go(func() {
		var wgInner sync.WaitGroup

		var sinks []messengerSink

		// Registers a sink and drains it in a tracked goroutine.
		start := func(queueName []byte, run func(ch <-chan msgtypes.Message)) {
			ch := make(chan msgtypes.Message, messengerBufLen)
			sinks = append(sinks, messengerSink{ch: ch, queue: queueName})

			wgInner.Add(1)

			wgMsg.Go(func() {
				defer wgInner.Done()

				run(ch)
			})
		}

		// Close before wait — see doc comment.
		defer func() {
			for _, s := range sinks {
				close(s.ch)
			}

			wgInner.Wait()
		}()

		// LIFO: runs before the close/wait defer, so the drain finishes while
		// this stage still owns gradesMsg.
		defer func() {
			if r := recover(); r != nil {
				recoverFanOut(ctx, eDB, sinks, gradesMsg, r)
			}
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

		if cfg.CalDAVEnabled {
			start(messenger.CalDAVQueueName, func(ch <-chan msgtypes.Message) {
				if err := messenger.CalDAV(ctx, eDB, ch, messenger.CalDAVConfig{
					URL:      cfg.CalDAV.URL,
					Username: cfg.CalDAV.Username,
					Password: cfg.CalDAV.Password,
					Retries:  *retries,
				}); err != nil {
					flagMessengerError(ctx, ErrCalDAV, err)
				}
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

		// Ends when gradesMsg closes.
		for g := range gradesMsg {
			for _, s := range sinks {
				dispatchFn(ctx, eDB, s, g)
			}
		}
	})
}

// recoverFanOut costs the cycle instead of the process, mirroring
// recoverMessenger.
//
// The drain is mandatory: msgDedup's handoff is a blocking send, so a fan-out
// that stops reading wedges it and runPollCycle never returns — a silent hang
// in place of a visible crash. Drained messages are already dedup-flagged and
// will never be re-scraped, so they go to the queues.
func recoverFanOut(ctx context.Context, eDB *sqlitedb.Edb, sinks []messengerSink, gradesMsg <-chan msgtypes.Message, r any) {
	logger.Error().Msgf("%v, spilling undelivered messages to the queues: %v", ErrFanOutPanic, r)
	exitWithError.Store(true)

	spilled := 0

	for g := range gradesMsg {
		spillAll(ctx, eDB, sinks, g)

		spilled++
	}

	if spilled > 0 {
		logger.Warn().Msgf("Spilled %v messages to the messenger queues after the fan-out failed", spilled)
	}
}

// spillAll queues g for every messenger, containing its own panic so a failing
// store cannot abort the caller's drain. Losing one message beats losing every
// later one to a deadlock.
func spillAll(ctx context.Context, eDB *sqlitedb.Edb, sinks []messengerSink, g msgtypes.Message) {
	defer func() {
		if r := recover(); r != nil {
			logger.Error().Msgf("%v: dropping a message while spilling: %v", ErrFanOutPanic, r)
		}
	}()

	for _, s := range sinks {
		spill(ctx, eDB, s, g)
	}
}

// messengerSink is a messenger's fan-out channel plus the queue for spills.
type messengerSink struct {
	ch    chan msgtypes.Message
	queue []byte
}

// dispatch delivers g to one messenger without ever blocking the fan-out. A
// full buffer means that messenger is behind (mail mid-retry, say), so the
// message spills to its queue rather than pacing every other messenger.
// Trade-off: it arrives a cycle late and slightly out of order.
func dispatch(ctx context.Context, eDB *sqlitedb.Edb, s messengerSink, g msgtypes.Message) {
	select {
	case s.ch <- g:
	default:
		spill(ctx, eDB, s, g)
	}
}

// dispatchFn is a test seam (cf. scrapeStage): a fan-out panic is reachable
// only through dispatch, and forcing one otherwise means racing a failing
// backend to fill its buffer.
var dispatchFn = dispatch

// spill queues g for one messenger, to deliver next cycle.
func spill(ctx context.Context, eDB *sqlitedb.Edb, s messengerSink, g msgtypes.Message) {
	// A row this messenger would discard is one nothing can consume.
	if !messenger.QueueAccepts(s.queue, g) {
		return
	}

	storeOverflow(ctx, eDB, s.queue, g)
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

// msgDedup forwards only events the dedup store has not seen before, and only
// once past the first run — a fresh database seeds silently instead of
// flooding.
func msgDedup(ctx context.Context, eDB *sqlitedb.Edb, wgFilter *sync.WaitGroup, gradesScraped <-chan msgtypes.Message, gradesMsg chan<- msgtypes.Message) {
	wgFilter.Go(func() {
		// Close gradesMsg on exit so msgSend's fan-out loop unblocks.
		defer close(gradesMsg)

		// Marked seen but not yet handed over. Dedup never re-fires a flagged
		// event, so a panic in that window must forward it (cf.
		// recoverMessenger's inflight).
		var flagged *msgtypes.Message

		// LIFO: runs before the close, while gradesScraped is still owned and
		// gradesMsg still open.
		defer func() {
			if r := recover(); r != nil {
				recoverDedup(gradesScraped, gradesMsg, flagged, r)
			}
		}()

		if !eDB.Existing() {
			logger.Info().Msg("Newly initialized database, won't send alerts in this run")
		}

		now := time.Now()

		for g := range gradesScraped {
			// Previous iteration reached a decision.
			flagged = nil

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

			// Flagged: every path from here forwards or deliberately
			// suppresses, so a panic in between must forward.
			flagged = &g

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

			flagged = nil
		}
	})
}

// recoverDedup costs the cycle instead of the process. The asymmetry between
// its two halves is the point:
//
//   - flagged is already committed as seen and can never re-fire, so dropping
//     it loses that alert permanently. It is forwarded; the blocking send is
//     safe because msgSend runs until gradesMsg closes, which happens after.
//   - The gradesScraped backlog never reached CheckAndFlagTTL, so it is
//     unflagged and discarded for the next cycle to re-scrape.
//
// The drain is mandatory either way: the scrapers' send yields only to
// ctx.Done(), which a panic never triggers, so a stage that stops reading
// parks them on a full channel and wgScrape.Wait() never returns.
func recoverDedup(gradesScraped <-chan msgtypes.Message, gradesMsg chan<- msgtypes.Message, flagged *msgtypes.Message, r any) {
	logger.Error().Msgf("%v: %v", ErrDedupPanic, r)
	exitWithError.Store(true)

	// Already seen: forward it or lose it.
	if flagged != nil {
		logger.Warn().Msgf("Forwarding the already-flagged event interrupted by the failure: %v/%v",
			flagged.Username, flagged.Subject)

		gradesMsg <- *flagged
	}

	discarded := 0
	for range gradesScraped {
		discarded++
	}

	if discarded > 0 {
		logger.Warn().Msgf("Discarded %v unflagged events after the dedup stage failed; they will be re-scraped next cycle",
			discarded)
	}
}

// maxLeapYearLookback bounds resolveHRYear's walk. 29 February is the only
// parseable date some years lack, and it does not recur every four — a non-leap
// century (1900, 2100) stretches the worst case to seven steps.
const maxLeapYearLookback = 8

// resolveHRYear dates a "D.M." column to the latest year in which it has
// already passed. The walk exists for 29 February: time.Date rolls it to 1
// March in a common year, making an older leap-year grade read as days old.
//
// Reports false when no year in range holds the date; callers must fail open.
// Returning a normalised date instead would read as years old and suppress the
// alert silently.
func resolveHRYear(t, now time.Time) (time.Time, bool) {
	year := now.Year()

	// Still ahead of today: it belongs to last year.
	if t.Month() > now.Month() || (t.Month() == now.Month() && t.Day() > now.Day()) {
		year--
	}

	for range maxLeapYearLookback {
		// Normalisation is what signals the date is absent.
		if d := time.Date(year, t.Month(), t.Day(), 0, 0, 0, 0, t.Location()); d.Day() == t.Day() {
			return d, true
		}

		year--
	}

	return time.Time{}, false
}

// isStaleEvent reports whether g should be suppressed as too old. Only Exam and
// Grade are time-filtered; every other code, and a zero relevancePeriod, counts
// as fresh. An unparseable date fails open — a stale alert beats a silent drop.
// Logging lives here so the caller stays a flat guard.
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
		// XXX Fields[0] is assumed to be the grade date. cellValues pads empty
		// cells, so a blank is alignment rather than drift — hence Debug, or a
		// subject with a blank first column logs forever.
		if g.Fields[0] == "" {
			logger.Debug().Msgf("No date to judge relevance for: %v/%v", g.Username, g.Subject)

			return false
		}

		t, err := time.Parse(formatHRDateOnly, g.Fields[0])
		if err != nil {
			// Fail open: a stale alert beats a silent drop.
			logger.Error().Msgf("Unable to parse date for: %v/%v: %+v: %v", g.Username, g.Subject, g, err)

			return false
		}

		resolved, ok := resolveHRYear(t, now)
		if !ok {
			// Fail open: a stale alert beats a silent drop.
			logger.Error().Msgf("Unable to place %q in a year for: %v/%v", g.Fields[0], g.Username, g.Subject)

			return false
		}

		if time.Since(resolved) > *relevancePeriod {
			logger.Warn().Msgf("Ignoring changes in an old event: %v/%v: %+v", g.Username, g.Subject, g)

			return true
		}
	}

	return false
}

// spinner runs until done is closed, on stderr so JSON logs on stdout stay
// parseable.
func spinner(done <-chan struct{}) {
	s := spin.New()

	for {
		fmt.Fprintf(os.Stderr, "\rWaiting... %v", s.Next())

		// Cancellable so shutdown isn't held by an in-flight sleep.
		select {
		case <-done:
			fmt.Fprint(os.Stderr, "\r")

			return
		case <-time.After(spinnerRotateDelay):
		}
	}
}

// versionCheck notes a newer GitHub release, skipping local and dirty builds.
// Bounded by versionCheckTimeout.
func versionCheck(ctx context.Context, wgVersion *sync.WaitGroup) {
	wgVersion.Go(func() {
		// Local build: the user owns their own version.
		if GitTag == "" || GitDirty != "" {
			return
		}

		currentTag, err := semver.NewVersion(strings.TrimPrefix(GitTag, "v"))
		if err != nil || currentTag == nil {
			logger.Error().Msgf("Unable to parse current version of e-dnevnik-bot: %v", err)

			return
		}

		// A stalled API must not outlive the poll cycle.
		vctx, cancel := context.WithTimeout(ctx, versionCheckTimeout)
		defer cancel()

		client, err := githubClient()
		if err != nil {
			logger.Error().Msgf("Unable to create GitHub client: %v", err)

			return
		}

		latestRelease, _, err := client.Repositories.GetLatestRelease(vctx, githubOrg, githubRepo)
		if err != nil || latestRelease == nil {
			// Shutdown cancelling mid-request is not an app error.
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

// githubClient authenticates via GITHUB_TOKEN when set.
func githubClient() (*github.Client, error) {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return github.NewClient(github.WithAuthToken(token))
	}

	return github.NewClient()
}
