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
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/avast/retry-go/v4"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/oauth"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

const (
	CalendarSendDelay  = 200 * time.Millisecond // recommended delay between Calendar API calls
	CalendarMaxResults = 200
)

var (
	ErrCalendarReadingCreds = errors.New("unable to read credentials file")
	ErrCalendarParsingCreds = errors.New("unable to parse credentials file")
	ErrCalendarNotFound     = errors.New("unable to find Google Calendar ID")
)

// Calendar is a function that processes events from a channel and inserts them into a Google Calendar.
//
// It takes in the following parameters:
//   - ctx: the context.Context object for handling timeouts and cancellations.
//   - ch: a channel of interface{} that contains the events to be processed.
//   - name: the name of the Google Calendar.
//   - tokFile: the path to the token file.
//   - credFile: the path to the credentials file.
//   - retries: the number of retry attempts for inserting a Google Calendar event.
//
// It returns an error if any of the following occurs:
//   - Unable to read the credentials file.
//   - Unable to parse the credentials file.
//   - Unable to initialize Google Calendar OAuth.
//   - Unable to initialize Google Calendar client.
//   - Unable to find the Google Calendar ID for the specified calendar.
//   - Unable to insert the Google Calendar event.
//
// The function reads the credentials file and configures the Google API client using the credentials.
// It then initializes the Google Calendar client and retrieves the Google Calendar ID for the specified calendar.
// Next, it processes the events received from the channel, formats the event description, and creates a new all-day event in the Google Calendar.
// Finally, it retries and attempts to insert the event into the Google Calendar, with a delay between each attempt.
//
// The function returns nil if all events are successfully processed and inserted into the Google Calendar.
func Calendar(ctx context.Context, ch <-chan interface{}, name, tokFile, credFile string, retries uint) error {
	srv, calID, err := initCalendar(ctx, credFile, tokFile, name)
	if err != nil {
		return err
	}

	logger.Debug().Msg("Creating exams with Google Calendar API")

	now := time.Now()

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

			// skip non-exam events and events in the past
			if !g.IsExam || g.Timestamp.Before(now) {
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

			// retryable and cancellable attempt
			err = retry.Do(
				func() error {
					_, err := srv.Events.Insert(calID, newEvent).Do()

					return err
				},
				retry.Attempts(retries),
				retry.Context(ctx),
			)
			if err != nil {
				logger.Error().Msgf("Unable to insert Google Calendar event: %v", err)
			}

			time.Sleep(CalendarSendDelay)
		}
	}

	return nil
}

// initCalendar initializes a Google Calendar service and retrieves the calendar ID.
//
// ctx: The context.Context for the function.
// credFile: The path to the credentials file.
// tokFile: The path to the token file.
// name: The name of the calendar.
// returns:
// - *calendar.Service: A pointer to the calendar.Service.
// - string: The calendar ID.
// - error: Any error that occurred during initialization.
func initCalendar(ctx context.Context, credFile string, tokFile string, name string) (*calendar.Service, string, error) {
	b, err := os.ReadFile(credFile)
	if err != nil {
		logger.Error().Msgf("Unable to read credentials file %s: %v", credFile, err)

		return nil, "", ErrCalendarReadingCreds
	}

	var config *oauth2.Config

	config, err = google.ConfigFromJSON(b, calendar.CalendarReadonlyScope, calendar.CalendarEventsScope)
	if err != nil {
		logger.Error().Msgf("Unable to parse credentials file %s: %v", credFile, err)

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
		logger.Error().Msgf("Unable to find Google Calendar ID for calendar %s", name)

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
			log.Fatalf("Unable to retrieve user's calendar: %v", err)
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
