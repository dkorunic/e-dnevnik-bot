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
	"sync"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/fetch"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"

	"github.com/avast/retry-go/v4"
)

// GetGradesAndEvents initiates fetching subjects, grades and exam events from remote e-dnevnik site, sends
// individual messages to a message channel and optionally returning an error.
func GetGradesAndEvents(ctx context.Context, msgPool *sync.Pool, ch chan<- *msgtypes.Message, username, password string,
	retries uint,
) error {
	err := func() error {
		ctx, stop := context.WithTimeout(ctx, time.Duration(retries)*fetch.Timeout)
		defer stop()

		client, err := fetch.NewClientWithContext(ctx, username, password)
		if err != nil {
			return err
		}

		// attempt to fetch subjects/grades/exams
		var rawGrades string
		var events fetch.Events
		err = retry.Do(
			func() error {
				var err error
				rawGrades, events, err = client.GetResponse()

				return err
			},
			retry.Attempts(retries),
			retry.Context(ctx),
		)
		if err != nil {
			return err
		}

		// parse all subjects and corresponding grades
		err = parseGrades(msgPool, ch, username, rawGrades)
		if err != nil {
			return err
		}

		// parse all exam events
		err = parseEvents(msgPool, ch, username, events)

		return err
	}()

	return err
}
