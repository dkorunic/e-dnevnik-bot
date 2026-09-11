// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"context"
	"errors"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/fetch"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// scrapeRetryMaxJitter caps backoff jitter to smooth simultaneous reconnect storms.
const scrapeRetryMaxJitter = 500 * time.Millisecond

// scrapeMaxAttempts caps retries so attempts*fetch.Timeout cannot overflow int64 nanoseconds.
const scrapeMaxAttempts = 100

// maxSessionRecoveries caps consecutive re-logins per scrape. A drifted auth
// marker makes every endpoint report expiry, which without a ceiling becomes a
// login POST per attempt per step — the rate limiter markPermanent avoids.
//
// Consecutive rather than total: -r up to 100 lets budgetCtx span hours and
// outlive several sessions legitimately.
const maxSessionRecoveries = 2

// recoverSession re-authenticates on a lapsed session and re-runs fn in place,
// so recovery survives retry-go's final attempt (-r 1 makes every attempt the
// final one). budget spans the whole scrape; see maxSessionRecoveries.
func recoverSession(fn, login func() error, budget *int, username string) error {
	for {
		err := fn()

		if err == nil {
			// Progress proves the session live; only unbroken failure means drift.
			*budget = maxSessionRecoveries

			return nil
		}

		if !errors.Is(err, fetch.ErrSessionExpired) {
			return err
		}

		if *budget <= 0 {
			logger.Error().Msgf("Portal keeps reporting an expired session for user %v after %d consecutive "+
				"re-authentications; giving up this cycle rather than retrying into the login rate limiter",
				username, maxSessionRecoveries)

			return retry.Unrecoverable(err)
		}

		*budget--

		logger.Warn().Msgf("Portal session expired for user %v, re-authenticating", username)

		if lerr := login(); lerr != nil {
			return markPermanent(lerr)
		}
	}
}

// markPermanent wraps fetch-level errors that cannot succeed on retry in
// retry.Unrecoverable so retry-go short-circuits the remaining attempts:
//   - ErrInvalidLogin: bad credentials — retrying just re-submits the same
//     POST and re-trips the portal's rate limiter.
//   - ErrAuthMarkerMissing: login page named no error and carried no marker —
//     either drifted alert markup with bad credentials, or a drifted marker on a
//     successful login. Retrying fixes neither.
//   - ErrBodyTooLarge: response exceeded MaxBodySize — a deterministic
//     server/content condition, not a transient network fault.
//
// fetch.ErrSessionExpired is absent by design: recoverSession re-authenticates,
// and marks it unrecoverable once its budget is spent.
func markPermanent(err error) error {
	if err == nil {
		return nil
	}

	if errors.Is(err, fetch.ErrInvalidLogin) ||
		errors.Is(err, fetch.ErrAuthMarkerMissing) ||
		errors.Is(err, fetch.ErrBodyTooLarge) {
		return retry.Unrecoverable(err)
	}

	return err
}

// GetGradesAndEvents initiates fetching subjects, grades and exam events from remote e-dnevnik site, sends
// individual messages to a message channel and optionally returning an error.
func GetGradesAndEvents(ctx context.Context, ch chan<- msgtypes.Message, username, password string, retries uint) error {
	// Caller passes a flag-clamped value >= 1; cap only the top so
	// attempts*fetch.Timeout can't overflow int64 nanoseconds.
	attempts := min(retries, scrapeMaxAttempts)

	r64 := int64(attempts)

	// One deadline for the whole per-user scrape. NOTE: this couples -r to total
	// pipeline time, not per-request retries — a large -r (up to 100) permits a
	// multi-hour cycle and can starve later steps. Keep -r modest.
	budgetCtx, stop := context.WithTimeout(ctx, time.Duration(r64)*fetch.Timeout)
	defer stop()

	client, err := fetch.NewClientWithContext(budgetCtx, username, password)
	if err != nil {
		return err
	}

	defer client.CloseConnections()

	recoveries := maxSessionRecoveries

	// Every scrape step shares one retry policy; close over attempts/budgetCtx.
	withRetry := func(fn func() error) error {
		return retry.New(
			retry.Attempts(attempts),
			retry.Context(budgetCtx),
			retry.DelayType(retry.BackOffDelay),
			retry.MaxJitter(scrapeRetryMaxJitter),
		).Do(func() error {
			return recoverSession(fn, client.Login, &recoveries, username)
		})
	}

	err = withRetry(func() error {
		return markPermanent(client.Login())
	})
	if err != nil {
		return err
	}

	var rawClasses []byte

	err = withRetry(func() error {
		var err error
		rawClasses, err = client.GetClasses()

		return markPermanent(err)
	})
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

		err = withRetry(func() error {
			var err error
			rawGrades, events, err = client.GetClassEvents(cID)

			return markPermanent(err)
		})
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

		err = withRetry(func() error {
			var err error
			rawCourses, err = client.GetCourses()

			return markPermanent(err)
		})
		if err != nil {
			return err
		}

		var subjects fetch.Courses

		subjects, err = parseCourses(username, rawCourses)
		if err != nil {
			return err
		}

		var rawCourse []byte

		for _, s := range subjects {
			err = withRetry(func() error {
				var err error
				rawCourse, err = client.GetCourse(s.URL)

				return markPermanent(err)
			})
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
