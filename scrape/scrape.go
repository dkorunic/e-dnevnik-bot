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
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/fetch"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/reiver/go-cast"
)

// GetGradesAndEvents initiates fetching subjects, grades and exam events from remote e-dnevnik site, sends
// individual messages to a message channel and optionally returning an error.
func GetGradesAndEvents(ctx context.Context, ch chan<- msgtypes.Message, username, password string, retries uint) error {
	err := func() error {
		r64, err := cast.Int64(retries)
		if err != nil {
			r64 = 1
		}

		ctx, stop := context.WithTimeout(ctx, time.Duration(r64)*fetch.Timeout)
		defer stop()

		client, err := fetch.NewClientWithContext(ctx, username, password)
		if err != nil {
			return err
		}

		defer client.CloseConnections()

		// attempt to login (CSRF, SSO/SAML, etc.)
		err = retry.New(
			retry.Attempts(retries),
			retry.Context(ctx),
		).Do(
			func() error {
				return client.Login()
			},
		)
		if err != nil {
			return err
		}

		var rawClasses string

		// fetch classes (multiple classes possible)
		err = retry.New(
			retry.Attempts(retries),
			retry.Context(ctx),
		).Do(
			func() error {
				var err error
				rawClasses, err = client.GetClasses()

				return err
			},
		)
		if err != nil {
			return err
		}

		// parse active classes
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

		// iterate all active classes
		for _, c := range classes {
			cID := c.ID
			cName := c.Name

			logger.Debug().Msgf("Fetching grades and calendar events for user %v, class %v, class ID %v", username,
				cName, cID)

			var rawGrades string

			var events fetch.Events

			// fetch subjects/grades/exams
			err = retry.New(
				retry.Attempts(retries),
				retry.Context(ctx),
			).Do(
				func() error {
					var err error
					rawGrades, events, err = client.GetClassEvents(cID)

					return err
				},
			)
			if err != nil {
				return err
			}

			// parse all subjects and corresponding grades
			err = parseGrades(ch, username, rawGrades, multiClass, cName)
			if err != nil {
				return err
			}

			// parse all exam events
			err = parseEvents(ch, username, events, multiClass, cName)
			if err != nil {
				return err
			}

			var rawCourses string

			// fetch individual courses
			err = retry.New(
				retry.Attempts(retries),
				retry.Context(ctx),
			).Do(
				func() error {
					var err error
					rawCourses, err = client.GetCourses()

					return err
				},
			)
			if err != nil {
				return err
			}

			var subjects fetch.Courses

			// parse all courses and reading lists and final grades
			subjects, err = parseCourses(rawCourses)
			if err != nil {
				return err
			}

			var rawCourse string

			for _, s := range subjects {
				// requires additional fetch
				rawCourse, err = client.GetCourse(s.URL)
				if err != nil {
					return err
				}

				// process individual course
				err = parseCourse(ch, username, rawCourse, multiClass, cName, s.Name)
				if err != nil {
					return err
				}
			}
		}

		return nil
	}()

	return err
}
