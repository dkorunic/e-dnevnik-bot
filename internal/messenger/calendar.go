// SPDX-FileCopyrightText: 2023 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"embed"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/oauth"
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

	// CalendarReminderMinutes is counted back from the all-day event's
	// midnight: 3 days before at 09:00, not at 00:00.
	CalendarReminderMinutes = (3*24 - 9) * 60
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

	var cs cycleSender

	resendQueued(ctx, eDB, CalendarQueueName, func(m msgtypes.Message) {
		cs.send(ctx, eDB, CalendarQueueName, m, func() bool {
			return processCalendar(ctx, eDB, m, rl, srv, calID, cfg.Retries)
		})
	})

	// Drain fully; processCalendar durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		inflight = &g
		cs.send(ctx, eDB, CalendarQueueName, g, func() bool {
			return processCalendar(ctx, eDB, g, rl, srv, calID, cfg.Retries)
		})
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
	// Same contract as the full messengers: degrade, don't crash. No inflight:
	// queueUndelivered's only panic-able step is the store itself.
	defer func() {
		if r := recover(); r != nil {
			logger.Error().Msgf("%v", recoverMessenger(ctx, eDB, CalendarQueueName, ch, r, nil))
		}
	}()

	// QueueAccepts keeps only exams for this queue.
	queueUndelivered(ctx, eDB, CalendarQueueName, ch)
}

// calendarRateReasons are the 403 reasons Google uses for rate and quota
// limits: transient, unlike every other 403.
var calendarRateReasons = []string{"rateLimitExceeded", "userRateLimitExceeded", "quotaExceeded", "dailyLimitExceeded"}

// markCalendarPermanent stops retry-go on a permanent 4xx. 5xx, transport
// errors and rate-limit 403s stay transient. 409 never gets here: the insert
// attempt reads it as success.
//
// Config errors, queued until re-authorised: a revoked or expired grant (the
// oauth2 transport's invalid_grant, which never becomes a googleapi.Error),
// a 401, and any other 403 — lost write access to the calendar is account-wide.
func markCalendarPermanent(err error) error {
	if err == nil {
		return nil
	}

	if re, ok := errors.AsType[*oauth2.RetrieveError](err); ok && re.ErrorCode == "invalid_grant" {
		return markConfigErr(err)
	}

	if gaErr, ok := errors.AsType[*googleapi.Error](err); ok {
		switch {
		case gaErr.Code == http.StatusUnauthorized:
			return markConfigErr(err)
		case gaErr.Code == http.StatusForbidden && slices.ContainsFunc(gaErr.Errors, func(e googleapi.ErrorItem) bool {
			return slices.Contains(calendarRateReasons, e.Reason)
		}):
			return err
		case gaErr.Code == http.StatusForbidden:
			return markConfigErr(err)
		}

		if isPermanentHTTPStatus(gaErr.Code) {
			// Inner sentinel survives retry.Do's marker stripping.
			return retry.Unrecoverable(permanentError{err})
		}
	}

	return err
}

// processCalendar inserts g as an all-day event, re-queueing on failure.
// examEventOf decides what is inserted and supplies the deterministic ID that
// makes a retried insert dedupe server-side.
func processCalendar(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, rl ratelimit.Limiter,
	srv *calendar.Service, calID string, retries uint,
) bool {
	var err error

	ev, ok := examEventOf("Google Calendar", g)
	if !ok {
		return false
	}

	// All-day event spanning a single date.
	newEvent := &calendar.Event{
		Id:          ev.ID,
		Summary:     ev.Summary,
		Description: ev.Description,
		Start: &calendar.EventDateTime{
			Date: ev.Date.Format(time.DateOnly),
		},
		End: &calendar.EventDateTime{
			Date: ev.Date.AddDate(0, 0, 1).Format(time.DateOnly),
		},
		// UseDefault=false is the zero value, so it must be force-sent or the
		// API rejects the overrides.
		Reminders: &calendar.EventReminders{
			Overrides: []*calendar.EventReminder{
				{Method: "popup", Minutes: CalendarReminderMinutes},
			},
			ForceSendFields: []string{"UseDefault"},
		},
	}

	// Cancelled before insert, or while waiting for a token: re-queue
	// rather than be dropped.
	if !takeCtx(ctx, rl) {
		storeQueued(ctx, eDB, CalendarQueueName, g)

		return false
	}

	err = retry.New(
		retry.Attempts(retries),
		retry.Context(ctx),
		retry.Delay(CalendarMinDelay),
	).Do(
		func() error {
			_, err := srv.Events.Insert(calID, newEvent).Context(ctx).Do()

			// 409: the deterministic event ID already exists, so the insert
			// landed — perhaps on an attempt that then timed out. Decided
			// here, not after retry.Do: errors.As on the aggregated error
			// finds the first attempt's error (cf. putCalDAV's 412).
			if gaErr, ok := errors.AsType[*googleapi.Error](err); ok && gaErr.Code == http.StatusConflict {
				logger.Debug().Msgf("Google Calendar event already exists (idempotent insert): %v", newEvent.Id)

				return nil
			}

			return markCalendarPermanent(err)
		},
	)
	if err != nil {
		if isPermanentSendErr(err) {
			// Permanent: drop, don't requeue.
			logger.Error().Msgf("Permanently dropping Google Calendar event for %v/%v (will not retry): %v",
				g.Username, g.Subject, err)

			return false
		}

		logger.Error().Msgf("Unable to insert Google Calendar event: %v", err)

		storeQueued(ctx, eDB, CalendarQueueName, g)

		return isConfigSendErr(err)
	}

	return false
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
	// Trimmed first, so " primary " is the alias too, not a failed lookup.
	want := strings.TrimSpace(calendarName)
	if want == "" || strings.EqualFold(want, CalendarPrimary) {
		return CalendarPrimary
	}
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
