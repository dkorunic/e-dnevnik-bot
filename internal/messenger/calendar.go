// SPDX-FileCopyrightText: 2023 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/oauth"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/dkorunic/e-dnevnik-bot/internal/version"
	"github.com/minio/sha256-simd"
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

// Calendar resends any queued failures, then inserts exam events from ch into
// the configured calendar. On init failure it drains ch into the queue so
// already-dedup-flagged events are not lost.
func Calendar(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message, cfg CalendarConfig) (err error) {
	// A send-path panic must drain ch and degrade, not crash the process.
	defer func() {
		if r := recover(); r != nil {
			err = recoverMessenger(ctx, eDB, CalendarQueueName, ch, r)
		}
	}()

	calendarMu.Lock()

	if calendarSrv == nil || calendarID == "" {
		calendarSrv, calendarID, err = InitCalendar(ctx, cfg.TokFile, cfg.Name)
		if err != nil {
			calendarMu.Unlock()

			// InitCalendar refreshes OAuth tokens over the network, so this
			// path is reachable on transient failures. Events are already
			// dedup-flagged; queue them or they are lost forever.
			queueUndelivered(ctx, eDB, CalendarQueueName, ch)

			return err
		}
	}

	calendarMu.Unlock()

	srv := calendarSrv
	calID := calendarID

	logger.Debug().Msgf("Started Google Calendar API messenger (%v)", CalendarVersion)

	rl := ratelimit.New(CalendarAPILimit, ratelimit.Per(CalendarWindow))

	// Resend queued failures first. Rows are only removed after processing, so
	// a crash mid-loop re-delivers instead of losing; on shutdown, unprocessed
	// rows simply stay queued for the next run.
	for _, q := range queue.FetchFailedMsgs(ctx, eDB, CalendarQueueName) {
		if ctx.Err() != nil {
			break
		}

		processCalendar(ctx, eDB, q.Msg, rl, srv, calID, cfg.Retries)

		// Failures were re-queued by processCalendar; drop the original row.
		queue.Dequeue(ctx, eDB, q.Key)
	}

	// Drain fully; processCalendar durably queues on cancelled ctx, losing nothing.
	for g := range ch {
		processCalendar(ctx, eDB, g, rl, srv, calID, cfg.Retries)
	}

	return nil
}

// CalendarDeferred is the queue-only stub msgSend runs when Calendar is
// configured but not yet initializable (headless daemon before interactive
// OAuth). It queues exam events — the only type Calendar delivers — so they are
// inserted once OAuth completes rather than dedup-flagged and lost; non-exam
// events are dropped (other messengers already got them).
func CalendarDeferred(ctx context.Context, eDB *sqlitedb.Edb, ch <-chan msgtypes.Message) {
	queued := 0

	for g := range ch {
		if g.Code != msgtypes.Exam {
			continue
		}

		// Shutdown-tolerant: queue write must survive ctx cancel.
		sctx, scancel := queueStoreCtx(ctx)
		if err := queue.StoreFailedMsgs(sctx, eDB, CalendarQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()

		queued++
	}

	if queued > 0 {
		logger.Warn().Msgf("Google Calendar not yet initialized; queued %v exam events for delivery once OAuth is completed",
			queued)
	}
}

// markCalendarPermanent marks permanent 4xx errors (except 408/429) as
// unrecoverable so retry-go stops retrying. 409 Conflict is permanent too and
// the caller treats it as success — the deterministic event ID means the
// insert already landed. 5xx/transport errors stay transient.
func markCalendarPermanent(err error) error {
	if err == nil {
		return nil
	}

	if gaErr, ok := errors.AsType[*googleapi.Error](err); ok {
		if isPermanentHTTPStatus(gaErr.Code) {
			return retry.Unrecoverable(err)
		}
	}

	return err
}

// processCalendar inserts g as an all-day event, re-queueing on failure.
// Non-exam events, past exams, and field-less exams are skipped. The event ID
// is a deterministic hash so a retried insert dedupes server-side (409).
func processCalendar(ctx context.Context, eDB *sqlitedb.Edb, g msgtypes.Message, rl ratelimit.Limiter,
	srv *calendar.Service, calID string, retries uint,
) {
	var err error

	// Calendar only receives exams; everything else is a no-op.
	if g.Code != msgtypes.Exam {
		logger.Debug().Msgf("Calendar: skipping non-exam event for %v/%v (code %v)", g.Username, g.Subject, g.Code)

		return
	}

	// Refresh per call: long-running daemons must not use a stale boundary.
	// Compare calendar dates, not instants: exam timestamps are midnight-UTC
	// all-day markers, so comparing against time.Now() directly would drop an
	// exam first seen on the exam day itself. Only strictly-past days skip.
	if g.Timestamp.Format(time.DateOnly) < time.Now().UTC().Format(time.DateOnly) {
		logger.Info().Msgf("Skipping old exam event for %v/%v: %+v", g.Username, g.Subject, g)

		return
	}

	if len(g.Fields) == 0 {
		logger.Warn().Msgf("Calendar: skipping exam event for %v/%v with no fields: %+v", g.Username, g.Subject, g)

		return
	}

	// Deterministic ID makes a retried insert a 409 (idempotent success below).
	// Keyed on (username, subject, date), NOT g.Fields — so a later edit to an
	// exam's note on the same date is a 409 no-op and keeps the original.
	// Accepted: exam notes rarely change once dated.
	idHash := sha256.Sum256(fmt.Appendf(nil, "%s\x00%s\x00%s",
		g.Username, g.Subject, g.Timestamp.Format(time.DateOnly)))

	// All-day event spanning a single date.
	newEvent := &calendar.Event{
		Id:      fmt.Sprintf("%x", idHash),
		Summary: g.Username + CalendarExamSep + g.Subject,
		Start: &calendar.EventDateTime{
			Date: g.Timestamp.Format(time.DateOnly),
		},
		End: &calendar.EventDateTime{
			Date: g.Timestamp.AddDate(0, 0, 1).Format(time.DateOnly),
		},
		Description: g.Fields[len(g.Fields)-1],
	}

	// Cancelled before insert: re-queue so caller's tail-slice does not drop us.
	if ctx.Err() != nil {
		// Shutdown-tolerant: queue write must survive ctx cancel.
		sctx, scancel := queueStoreCtx(ctx)
		if err = queue.StoreFailedMsgs(sctx, eDB, CalendarQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()

		return
	}

	rl.Take()

	// 409 Conflict short-circuits via Unrecoverable, then post-Do treats it as idempotent success.
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
			// Permanent non-409 (400/403/404): drop, don't requeue.
			logger.Error().Msgf("Permanently dropping Google Calendar event for %v/%v (will not retry): %v",
				g.Username, g.Subject, err)

			return
		}

		logger.Error().Msgf("Unable to insert Google Calendar event: %v", err)

		// Shutdown-tolerant: queue write must survive ctx cancel.
		sctx, scancel := queueStoreCtx(ctx)
		if err = queue.StoreFailedMsgs(sctx, eDB, CalendarQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		scancel()

		return
	}
}

// InitCalendar builds an OAuth2-authenticated Calendar service (running the
// interactive consent flow if tokFile has no valid token) and resolves the
// calendar named name to its ID.
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
func getCalendarID(ctx context.Context, srv *calendar.Service, calendarName string) string {
	// Empty name resolves to the user's primary calendar.
	if calendarName == "" {
		return "primary"
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
			if item.Summary == calendarName {
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
