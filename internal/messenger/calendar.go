// SPDX-FileCopyrightText: 2023 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/oauth"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/internal/version"
	"go.uber.org/ratelimit"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

const (
	CalendarAPILimit    = 20 // 20 req/min per user
	CalendarWindow      = 1 * time.Minute
	CalendarMinDelay    = CalendarWindow / CalendarAPILimit
	CalendarMaxResults  = 100
	CalendarCredentials = "assets/calendar_credentials.json" // embedded Google Calendar credentials file
	CalendarQueue       = "calendar-queue"

	CalendarExamSep = " - Ispit iz: "

	// CalendarPrimary is both the ID and the accepted alias of the primary
	// calendar.
	CalendarPrimary = "primary"
)

var (
	ErrCalendarReadingCreds = errors.New("unable to read credentials file")
	ErrCalendarParsingCreds = errors.New("unable to parse credentials file")
	ErrCalendarNotFound     = errors.New("unable to find Google Calendar ID")

	CalendarQueueName = []byte(CalendarQueue)
	CalendarVersion   = version.ReadVersion("google.golang.org/api")

	calendarSrv *calendar.Service // cached Google Calendar service, initialized once
	calendarID  string            // cached calendar ID, resolved once
	calendarMu  sync.Mutex        // guards calendarSrv and calendarID initialisation
)

//go:embed assets/calendar_credentials.json
var credentialFS embed.FS

// CalendarConfig holds the per-messenger settings for the Google Calendar backend.
type CalendarConfig struct {
	Name    string
	TokFile string
	Retries uint
}

// Calendar resends queued failures, then inserts ch's exams into the configured
// calendar. On init failure it drains ch to the queue rather than lose flagged
// events.
func Calendar(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg CalendarConfig) (err error) {
	// Panic guard; stays nil on the resend path (see recoverMessenger).
	var inflight *msgtypes.Message

	defer func() {
		if r := recover(); r != nil {
			err = recoverMessenger(ctx, eDB, CalendarQueueName, ch, r, inflight)
		}
	}()

	srv, calID, err := ensureCalendarInit(ctx, cfg.TokFile, cfg.Name)
	if err != nil {
		// Init does network I/O (OAuth refresh), so this may be transient.
		// Already dedup-flagged: queue them or lose them forever.
		queueUndelivered(ctx, eDB, CalendarQueueName, ch)

		return err
	}

	logger.Debug().Msgf("Started Google Calendar API messenger (%v)", CalendarVersion)

	rl := ratelimit.New(CalendarAPILimit, ratelimit.Per(CalendarWindow))

	resendQueued(ctx, eDB, CalendarQueueName, func(m msgtypes.Message) {
		processCalendar(ctx, eDB, m, rl, srv, calID, cfg.Retries)
	})

	// Drain fully; processCalendar durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		processCalendar(ctx, eDB, g, rl, srv, calID, cfg.Retries)
		inflight = nil
	}

	return nil
}

// ensureCalendarInit lazily initialises the shared service. The unlock is
// deferred, not manual: a panic in InitCalendar would otherwise leak the lock
// and deadlock the next cycle at Lock, hanging shutdown.
func ensureCalendarInit(ctx context.Context, tokFile, name string) (*calendar.Service, string, error) {
	calendarMu.Lock()
	defer calendarMu.Unlock()

	if calendarSrv == nil || calendarID == "" {
		srv, id, err := InitCalendar(ctx, tokFile, name)
		if err != nil {
			return nil, "", err
		}

		calendarSrv, calendarID = srv, id
	}

	return calendarSrv, calendarID, nil
}

// CalendarDeferred is the queue-only stub for a Calendar configured but not yet
// initialisable — a headless daemon before interactive OAuth. Exams are queued
// for insertion once OAuth completes rather than flagged and lost; anything else
// is dropped, the other messengers having already taken it.
func CalendarDeferred(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message) {
	// Same contract as the full messengers: degrade, don't crash.
	var inflight *msgtypes.Message

	defer func() {
		if r := recover(); r != nil {
			logger.Error().Msgf("%v", recoverMessenger(ctx, eDB, CalendarQueueName, ch, r, inflight))
		}
	}()

	queued := 0

	for g := range ch {
		if g.Code != msgtypes.Exam {
			continue
		}

		inflight = &g

		// Must survive ctx cancel.
		sctx, scancel := queueStoreCtx(ctx)
		if err := queue.StoreFailedMsgs(sctx, eDB, CalendarQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()

		inflight = nil

		queued++
	}

	if queued > 0 {
		logger.Warn().Msgf("Google Calendar not yet initialized; queued %v exam events for delivery once OAuth is completed",
			queued)
	}
}

// markCalendarPermanent stops retry-go on a permanent 4xx. 409 is permanent too
// but the caller reads it as success: the deterministic event ID means the
// insert already landed. 5xx and transport errors stay transient.
func markCalendarPermanent(err error) error {
	if err == nil {
		return nil
	}

	if gaErr, ok := errors.AsType[*googleapi.Error](err); ok {
		if isPermanentHTTPStatus(gaErr.Code) {
			// Inner sentinel survives retry.Do's marker stripping.
			return retry.Unrecoverable(permanentError{err})
		}
	}

	return err
}

// processCalendar inserts g as an all-day event, re-queueing on failure and
// skipping non-exams, past exams and field-less exams. The deterministic event
// ID makes a retried insert dedupe server-side.
func processCalendar(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, rl ratelimit.Limiter,
	srv *calendar.Service, calID string, retries uint,
) {
	var err error

	// Only exams are delivered here.
	if g.Code != msgtypes.Exam {
		logger.Debug().Msgf("Calendar: skipping non-exam event for %v/%v (code %v)", g.Username, g.Subject, g.Code)

		return
	}

	// Recomputed per call so a long-running daemon never uses a stale boundary,
	// and compared as dates rather than instants: exam timestamps are midnight-UTC
	// all-day markers, so an instant comparison would drop an exam first seen on
	// the day itself.
	if g.Timestamp.Format(time.DateOnly) < time.Now().UTC().Format(time.DateOnly) {
		logger.Info().Msgf("Skipping old exam event for %v/%v: %+v", g.Username, g.Subject, g)

		return
	}

	if len(g.Fields) == 0 {
		logger.Warn().Msgf("Calendar: skipping exam event for %v/%v with no fields: %+v", g.Username, g.Subject, g)

		return
	}

	// Keyed on (username, subject, date), not g.Fields, so a later edit to the
	// note on the same date is a 409 no-op that keeps the original. Accepted:
	// notes rarely change once dated.
	idHash := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s",
		g.Username, g.Subject, g.Timestamp.Format(time.DateOnly)))

	// All-day event spanning a single date.
	newEvent := &calendar.Event{
		Id:      hex.EncodeToString(idHash[:]),
		Summary: g.Username + CalendarExamSep + g.Subject,
		Start: &calendar.EventDateTime{
			Date: g.Timestamp.Format(time.DateOnly),
		},
		End: &calendar.EventDateTime{
			Date: g.Timestamp.AddDate(0, 0, 1).Format(time.DateOnly),
		},
	}

	// Third field of scrape's exam layout (subject, date, note). A short
	// row — a legacy queue entry — gets no description rather than a
	// mis-picked field.
	if len(g.Fields) >= 3 {
		newEvent.Description = g.Fields[2]
	}

	// Cancelled before insert: re-queue rather than be dropped.
	if ctx.Err() != nil {
		// Must survive ctx cancel.
		sctx, scancel := queueStoreCtx(ctx)
		if err = queue.StoreFailedMsgs(sctx, eDB, CalendarQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()

		return
	}

	rl.Take()

	// 409 short-circuits here and reads as idempotent success below.
	err = retry.New(
		retry.Attempts(retries),
		retry.Context(ctx),
		retry.Delay(CalendarMinDelay),
	).Do(
		func() error {
			_, err := srv.Events.Insert(calID, newEvent).Context(ctx).Do()

			return markCalendarPermanent(err)
		},
	)
	if err != nil {
		var gaErr *googleapi.Error
		if errors.As(err, &gaErr) && gaErr.Code == http.StatusConflict {
			logger.Debug().Msgf("Google Calendar event already exists (idempotent insert): %v", newEvent.Id)

			return
		}

		if isPermanentSendErr(err) {
			// Permanent and not a 409: drop, don't requeue.
			logger.Error().Msgf("Permanently dropping Google Calendar event for %v/%v (will not retry): %v",
				g.Username, g.Subject, err)

			return
		}

		logger.Error().Msgf("Unable to insert Google Calendar event: %v", err)

		// Must survive ctx cancel.
		sctx, scancel := queueStoreCtx(ctx)
		if err = queue.StoreFailedMsgs(sctx, eDB, CalendarQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()

		return
	}
}

// InitCalendar builds an authenticated Calendar service, running interactive
// consent if tokFile holds no valid token, and resolves name to a calendar ID.
func InitCalendar(ctx context.Context, tokFile, name string) (*calendar.Service, string, error) {
	b, err := credentialFS.ReadFile(CalendarCredentials)
	if err != nil {
		logger.Error().Msgf("Unable to read credentials file %s: %v", CalendarCredentials, err)

		return nil, "", ErrCalendarReadingCreds
	}

	var config *oauth2.Config

	config, err = google.ConfigFromJSON(b, calendar.CalendarReadonlyScope, calendar.CalendarEventsScope)
	if err != nil {
		logger.Error().Msgf("Unable to parse credentials file %s: %v", CalendarCredentials, err)

		return nil, "", ErrCalendarParsingCreds
	}

	var client *http.Client

	client, err = oauth.GetClient(ctx, config, tokFile)
	if err != nil {
		logger.Error().Msgf("Unable to initialize Google Calendar OAuth: %v", err)

		return nil, "", err
	}

	var srv *calendar.Service

	srv, err = calendar.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		logger.Error().Msgf("Unable to initialize Google Calendar client: %v", err)

		return nil, "", err
	}

	calID := getCalendarID(ctx, srv, name)
	if calID == "" {
		logger.Error().Msgf("Unable to find Google Calendar ID for calendar: %v", name)

		return nil, "", ErrCalendarNotFound
	}

	return srv, calID, nil
}

// getCalendarID gets a Google calendar ID out of a symbolic calendar name.
// An empty name or the literal "primary" resolves to the user's primary
// calendar (whose Summary is the owner's e-mail address, so it can never
// match by name). Name matching is case-insensitive and space-tolerant.
func getCalendarID(ctx context.Context, srv *calendar.Service, calendarName string) string {
	if calendarName == "" || strings.EqualFold(calendarName, CalendarPrimary) {
		return CalendarPrimary
	}

	want := strings.TrimSpace(calendarName)
	nextPageToken := ""

	for {
		calendarsCall := srv.CalendarList.List().
			MaxResults(CalendarMaxResults).
			PageToken(nextPageToken)

		listCal, err := calendarsCall.Context(ctx).Do()
		if err != nil {
			logger.Error().Msgf("Unable to retrieve user's calendar: %v", err)

			return ""
		}

		for _, item := range listCal.Items {
			if strings.EqualFold(strings.TrimSpace(item.Summary), want) {
				return item.Id
			}
		}

		nextPageToken = listCal.NextPageToken
		if nextPageToken == "" {
			break
		}
	}

	return ""
}
