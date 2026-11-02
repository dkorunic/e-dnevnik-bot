// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

const (
	quietClockFormat = "15:04"
	quietSeparator   = "-"
	quietOff         = "off"
	minutesPerHour   = 60
	minutesPerDay    = 24 * minutesPerHour
)

var (
	errQuietFormat = errors.New("expected HH:MM-HH:MM or off")
	errQuietEmpty  = errors.New("start and end must differ")
)

// quietWindow is a daily range of minutes since midnight, end exclusive, that
// wraps midnight when end is before start. start == end means no window.
//
// Wall-clock minutes rather than durations, so DST moves the window with the
// clock instead of shifting it by an hour.
type quietWindow struct {
	start, end int
}

// Set implements flag.Value. "off" rather than an empty value, which ff reads as
// a missing one.
func (q *quietWindow) Set(s string) error {
	if s == quietOff {
		*q = quietWindow{}

		return nil
	}

	from, to, ok := strings.Cut(s, quietSeparator)
	if !ok {
		return errQuietFormat
	}

	start, err := time.Parse(quietClockFormat, from)
	if err != nil {
		return errQuietFormat
	}

	end, err := time.Parse(quietClockFormat, to)
	if err != nil {
		return errQuietFormat
	}

	// Equal bounds could mean all day or never, and all day silently stops
	// every alert.
	if start.Equal(end) {
		return errQuietEmpty
	}

	*q = quietWindow{start: minuteOfDay(start), end: minuteOfDay(end)}

	return nil
}

// String implements flag.Value.
func (q *quietWindow) String() string {
	if q.start == q.end {
		return quietOff
	}

	return fmt.Sprintf("%02d:%02d%s%02d:%02d", q.start/minutesPerHour, q.start%minutesPerHour,
		quietSeparator, q.end/minutesPerHour, q.end%minutesPerHour)
}

// GetPlaceholder names the format in --help. ff keeps backticks in usage text
// when it takes a placeholder from there.
func (q *quietWindow) GetPlaceholder() string {
	return "HH:MM-HH:MM"
}

// contains reports whether t's wall clock falls inside the window.
func (q *quietWindow) contains(t time.Time) bool {
	now := minuteOfDay(t)

	if q.start <= q.end {
		return q.start <= now && now < q.end
	}

	return now >= q.start || now < q.end
}

func minuteOfDay(t time.Time) int {
	return t.Hour()*minutesPerHour + t.Minute()
}

// pollSkipped reports whether a poll due at now should not run. Daemon only: a
// one-shot run was asked for explicitly, by a person or by a cron schedule that
// already decides when it fires.
func pollSkipped(now time.Time) bool {
	return *daemon && quietHours.contains(now)
}

// pollRuns reports whether a tick due at now polls. The first always does: it
// validates credentials and scraping, and a bad password should not wait for
// morning to show up in the logs.
func pollRuns(now time.Time, first bool) bool {
	return first || !pollSkipped(now)
}

// nextPollDelay returns how long to wait for the next poll. One due inside the
// window waits for its end instead, or an interval near a multiple of a day
// could land inside it every time and never poll.
//
// The wake-up jitter stays within the active gap: past it, the tick lands in
// the next window. Continuous, like durationRandJitter.
func nextPollDelay(now time.Time) time.Duration {
	delay := *tickInterval
	if *jitter {
		delay = durationRandJitter(delay)
	}

	due := now.Add(delay)
	if !*daemon || !quietHours.contains(due) {
		return delay
	}

	delay += quietHours.untilEnd(due)

	if *jitter {
		//nolint:gosec,mnd
		delay += time.Duration(rand.Float64() * float64(min(*tickInterval/10, quietHours.activeSpan())))
	}

	return delay
}

// untilEnd returns the time from t, which must be inside the window, to its end.
// Built from the wall clock, so DST moves the instant rather than the time of
// day.
func (q *quietWindow) untilEnd(t time.Time) time.Duration {
	y, m, d := t.Date()

	end := time.Date(y, m, d, q.end/minutesPerHour, q.end%minutesPerHour, 0, 0, t.Location())
	if !end.After(t) {
		end = end.AddDate(0, 0, 1)
	}

	return end.Sub(t)
}

// activeSpan returns the daily time outside the window.
func (q *quietWindow) activeSpan() time.Duration {
	if q.start == q.end {
		return minutesPerDay * time.Minute
	}

	return time.Duration((q.start-q.end+minutesPerDay)%minutesPerDay) * time.Minute
}

// describe renders the window for the startup banner. The zone is the only sign
// of a container running it in UTC because TZ is unset.
func (q *quietWindow) describe(t time.Time) string {
	if q.start == q.end {
		return q.String()
	}

	zone, _ := t.Zone()

	return q.String() + " " + zone
}
