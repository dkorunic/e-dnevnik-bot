// @license
// Copyright (C) 2023  Dinko Korunic
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

package messenger

import (
	"context"
	"embed"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/oauth"
	"github.com/dkorunic/e-dnevnik-bot/queue"
	"github.com/dkorunic/e-dnevnik-bot/version"
	"go.uber.org/ratelimit"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
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
)

//go:embed assets/calendar_credentials.json
var credentialFS embed.FS

// Calendar sends messages through the Google Calendar API to the specified calendar.
//
// It takes the following parameters:
// - ctx: the context.Context object for handling deadlines and cancellations.
// - eDB: the database instance for checking failed messages.
// - ch: a channel for receiving messages to be sent.
// - name: the name of the calendar to be used.
// - tokFile: the path to the file containing the OAuth2 token.
// - retries: the number of times to retry sending a message in case of failure.
//
// It returns an error indicating any failures that occurred during the process.
func Calendar(ctx context.Context, eDB *db.Edb, ch <-chan msgtypes.Message, name, tokFile string, retries uint) error {
	srv, calID, err := InitCalendar(ctx, tokFile, name)
	if err != nil {
		return err
	}

	logger.Debug().Msgf("Started Google Calendar API messenger (%v)", CalendarVersion)

	now := time.Now()
	rl := ratelimit.New(CalendarAPILimit, ratelimit.Per(CalendarWindow))

	var g msgtypes.Message

	// process all failed messages
	for _, g = range queue.FetchFailedMsgs(eDB, CalendarQueueName) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processCalendar(ctx, eDB, g, now, rl, srv, calID, retries)
		}
	}

	// process all messages
	for g = range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			processCalendar(ctx, eDB, g, now, rl, srv, calID, retries)
		}
	}

	return nil
}

// processCalendar is a helper function that processes a message from a channel and creates an event in Google Calendar.
//
// It takes the following parameters:
// - ctx: the context.Context object for managing the execution of the function
// - eDB: the database instance for checking failed messages
// - g: the message to be processed
// - now: the current time
// - rl: the rate limiter
// - srv: the Google Calendar service client
// - calID: the ID of the Google Calendar
// - retries: the number of retry attempts for inserting a Google Calendar event
//
// It returns an error indicating any issues encountered during the execution of the function.
func processCalendar(ctx context.Context, eDB *db.Edb, g msgtypes.Message, now time.Time, rl ratelimit.Limiter,
	srv *calendar.Service, calID string, retries uint,
) {
	var err error

	// skip non-exam events
	if g.Code != msgtypes.Exam {
		return
	}

	// skip events in the past
	if g.Timestamp.Before(now) {
		logger.Info().Msgf("Skipping old exam event for %v/%v: %+v", g.Username, g.Subject, g)

		return
	}

	// create an all day event
	newEvent := &calendar.Event{
		Summary: strings.Join([]string{g.Username, g.Subject}, CalendarExamSep),
		Start: &calendar.EventDateTime{
			Date: g.Timestamp.Format(time.DateOnly),
		},
		End: &calendar.EventDateTime{
			Date: g.Timestamp.AddDate(0, 0, 1).Format(time.DateOnly),
		},
		Description: g.Fields[len(g.Fields)-1],
	}

	rl.Take()

	// retryable and cancellable attempt
	err = retry.New(
		retry.Attempts(retries),
		retry.Context(ctx),
		retry.Delay(CalendarMinDelay),
	).Do(
		func() error {
			_, err := srv.Events.Insert(calID, newEvent).Do()

			return err
		},
	)
	if err != nil {
		logger.Error().Msgf("Unable to insert Google Calendar event: %v", err)

		// store failed message
		if err = queue.StoreFailedMsgs(eDB, CalendarQueueName, g); err != nil {
			logger.Error().Msgf("%v: %v", queue.ErrQueueing, err)
		}

		return
	}
}

// InitCalendar initializes a Google Calendar service and retrieves the calendar ID.
//
// ctx: The context.Context for the function.
// tokFile: The path to the token file.
// name: The name of the calendar.
// returns:
// - *calendar.Service: A pointer to the calendar.Service.
// - string: The calendar ID.
// - error: Any error that occurred during initialization.
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

	calID := getCalendarID(srv, name)
	if calID == "" {
		logger.Error().Msgf("Unable to find Google Calendar ID for calendar: %v", name)

		return nil, "", ErrCalendarNotFound
	}

	return srv, calID, nil
}

// getCalendarID gets a Google calendar ID out of a symbolic calendar name.
func getCalendarID(srv *calendar.Service, calendarName string) string {
	// If the calendar name is not specified, use default (primary) calendar
	if calendarName == "" {
		return "primary"
	}

	nextPageToken := ""

	// Get calendar listing (paginated) and try to match name
	for {
		calendarsCall := srv.CalendarList.List().
			MaxResults(CalendarMaxResults).
			PageToken(nextPageToken)

		listCal, err := calendarsCall.Do()
		if err != nil {
			logger.Error().Msgf("Unable to retrieve user's calendar: %v", err)

			return ""
		}

		// Match calendar name
		for _, item := range listCal.Items {
			if item.Summary == calendarName {
				return item.Id
			}
		}

		// Handle pagination
		nextPageToken = listCal.NextPageToken
		if nextPageToken == "" {
			break
		}
	}

	return ""
}
