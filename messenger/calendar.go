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

	"github.com/avast/retry-go/v4"
	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/oauth"
	"go.uber.org/ratelimit"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

const (
	CalendarAPILimit    = 5 // 5 req/s per user
	CalendarWindow      = 1 * time.Second
	CalendarMinDelay    = CalendarWindow / CalendarAPILimit
	CalendarMaxResults  = 100
	CalendarCredentials = "assets/calendar_credentials.json" // embedded Google Calendar credentials file

)

var (
	ErrCalendarReadingCreds = errors.New("unable to read credentials file")
	ErrCalendarParsingCreds = errors.New("unable to parse credentials file")
	ErrCalendarNotFound     = errors.New("unable to find Google Calendar ID")

	CalendarQueueName = []byte("calendar-queue")
)

//go:embed assets/calendar_credentials.json
var credentialFS embed.FS

// Calendar is a function that processes messages from a channel and creates events in Google Calendar.
//
// It takes the following parameters:
// - ctx: the context.Context object for managing the execution of the function
// - eDB: the database instance for checking failed messages
// - ch: a channel for receiving messages
// - name: the name of the calendar
// - tokFile: the path to the token file
// - retries: the number of retry attempts for inserting a Google Calendar event
//
// It returns an error indicating any issues encountered during the execution of the function.
func Calendar(ctx context.Context, eDB *db.Edb, ch <-chan interface{}, name, tokFile string, retries uint) error {
	srv, calID, err := InitCalendar(ctx, tokFile, name)
	if err != nil {
		return err
	}

	logger.Debug().Msg("Started Google Calendar API messenger")

	now := time.Now()
	rl := ratelimit.New(CalendarAPILimit, ratelimit.Per(CalendarWindow))

	// process all messages
	for o := range ch {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			g, ok := o.(msgtypes.Message)
			if !ok {
				logger.Warn().Msg("Received invalid type from channel, trying to continue")

				continue
			}

			// skip non-exam events
			if !g.IsExam {
				continue
			}

			// skip events in the past
			if g.Timestamp.Before(now) {
				logger.Info().Msgf("Skipping old exam event for %v/%v: %+v", g.Username, g.Subject, g)

				continue
			}

			// create an all day event
			newEvent := &calendar.Event{
				Summary: strings.Join([]string{g.Username, g.Subject}, " - Ispit iz: "),
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
			err = retry.Do(
				func() error {
					_, err := srv.Events.Insert(calID, newEvent).Do()

					return err
				},
				retry.Attempts(retries),
				retry.Context(ctx),
				retry.Delay(CalendarMinDelay),
			)
			if err != nil {
				logger.Error().Msgf("Unable to insert Google Calendar event: %v", err)

				// store failed message
				if err := storeFailedMsgs(eDB, CalendarQueueName, g); err != nil {
					logger.Error().Msgf("%v: %v", ErrQueueing, err)
				}

				continue
			}
		}
	}

	return nil
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
