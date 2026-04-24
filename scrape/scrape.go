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

package scrape

import (
	"context"
	"errors"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/fetch"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

// scrapeRetryMaxJitter bounds the random component added to exponential backoff
// between retry attempts, smoothing simultaneous reconnect storms.
const scrapeRetryMaxJitter = 500 * time.Millisecond

// scrapeMaxAttempts caps the retry budget so `attempts * fetch.Timeout` cannot
// overflow int64 nanoseconds if the config sets an absurdly large retries
// value. The product at the cap (100 * 120s = 200 min) is already far beyond
// any realistic scrape budget.
const scrapeMaxAttempts = 100

// markPermanent wraps fetch-level errors that cannot succeed on retry in
// retry.Unrecoverable so retry-go short-circuits the remaining attempts:
//   - ErrInvalidLogin: bad credentials — retrying just re-submits the same
//     POST and re-trips the portal's rate limiter.
//   - ErrBodyTooLarge: response exceeded MaxBodySize — a deterministic
//     server/content condition, not a transient network fault.
func markPermanent(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, fetch.ErrInvalidLogin) || errors.Is(err, fetch.ErrBodyTooLarge) {
		return retry.Unrecoverable(err)
	}

	return err
}

// GetGradesAndEvents initiates fetching subjects, grades and exam events from remote e-dnevnik site, sends
// individual messages to a message channel and optionally returning an error.
func GetGradesAndEvents(ctx context.Context, ch chan<- msgtypes.Message, username, password string, retries uint) error {
	// Clamp [1, scrapeMaxAttempts]: avoid retry-forever on 0 and duration overflow.
	attempts := min(max(retries, 1), uint(scrapeMaxAttempts))

	r64 := int64(attempts)

	// Shared deadline across entire scrape.
	budgetCtx, stop := context.WithTimeout(ctx, time.Duration(r64)*fetch.Timeout)
	defer stop()

	client, err := fetch.NewClientWithContext(budgetCtx, username, password)
	if err != nil {
		return err
	}

	defer client.CloseConnections()

	err = retry.New(
		retry.Attempts(attempts),
		retry.Context(budgetCtx),
		retry.DelayType(retry.BackOffDelay),
		retry.MaxJitter(scrapeRetryMaxJitter),
	).Do(
		func() error {
			return markPermanent(client.Login())
		},
	)
	if err != nil {
		return err
	}

	var rawClasses []byte

	err = retry.New(
		retry.Attempts(attempts),
		retry.Context(budgetCtx),
		retry.DelayType(retry.BackOffDelay),
		retry.MaxJitter(scrapeRetryMaxJitter),
	).Do(
		func() error {
			var err error
			rawClasses, err = client.GetClasses()

			return markPermanent(err)
		},
	)
	if err != nil {
		return err
	}

	classes, err := parseClasses(username, rawClasses)
	if err != nil {
		return err
	}

	multiClass := len(classes) > 1

	if multiClass {
		logger.Debug().Msgf("Found multiple active classes for user %v: %+v", username, classes)
	} else {
		logger.Debug().Msgf("Found active class for user %v: %+v", username, classes)
	}

	for _, c := range classes {
		cID := c.ID
		cName := c.Name

		logger.Debug().Msgf("Fetching grades and calendar events for user %v, class %v, class ID %v", username,
			cName, cID)

		var rawGrades []byte

		var events fetch.Events

		err = retry.New(
			retry.Attempts(attempts),
			retry.Context(budgetCtx),
			retry.DelayType(retry.BackOffDelay),
			retry.MaxJitter(scrapeRetryMaxJitter),
		).Do(
			func() error {
				var err error
				rawGrades, events, err = client.GetClassEvents(cID)

				return markPermanent(err)
			},
		)
		if err != nil {
			return err
		}

		err = parseGrades(budgetCtx, ch, username, rawGrades, multiClass, cName)
		if err != nil {
			return err
		}

		err = parseEvents(budgetCtx, ch, username, events, multiClass, cName)
		if err != nil {
			return err
		}

		var rawCourses []byte

		err = retry.New(
			retry.Attempts(attempts),
			retry.Context(budgetCtx),
			retry.DelayType(retry.BackOffDelay),
			retry.MaxJitter(scrapeRetryMaxJitter),
		).Do(
			func() error {
				var err error
				rawCourses, err = client.GetCourses()

				return markPermanent(err)
			},
		)
		if err != nil {
			return err
		}

		var subjects fetch.Courses

		subjects, err = parseCourses(rawCourses)
		if err != nil {
			return err
		}

		var rawCourse []byte

		for _, s := range subjects {
			err = retry.New(
				retry.Attempts(attempts),
				retry.Context(budgetCtx),
				retry.DelayType(retry.BackOffDelay),
				retry.MaxJitter(scrapeRetryMaxJitter),
			).Do(
				func() error {
					var err error
					rawCourse, err = client.GetCourse(s.URL)

					return markPermanent(err)
				},
			)
			if err != nil {
				return err
			}

			err = parseCourse(budgetCtx, ch, username, rawCourse, multiClass, cName, s.Name)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
