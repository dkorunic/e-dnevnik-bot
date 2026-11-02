// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"math/rand/v2"
	"testing"
	"time"
)

// An end the clock skips on the spring-forward day must not cost a poll: the old
// date arithmetic slid it an hour early, into the window, or a whole day late.
func TestNextPollDelayWindowEndingInTheDSTGap(t *testing.T) {
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("LoadLocation() = %v", err)
	}

	tests := []struct {
		now      time.Time
		want     time.Time
		name     string
		window   string
		interval time.Duration
	}{
		{
			name:     "landing after the gap day's end waits for the next day's",
			window:   "09:48-02:06",
			now:      time.Date(2026, time.March, 8, 5, 24, 6, 0, newYork),
			interval: 15 * time.Hour,
			want:     time.Date(2026, time.March, 9, 2, 6, 0, 0, newYork),
		},
		{
			name:     "landing before the gap ends with it, not a day later",
			window:   "22:00-02:30",
			now:      time.Date(2026, time.March, 8, 0, 30, 0, 0, newYork),
			interval: time.Hour,
			want:     time.Date(2026, time.March, 8, 3, 0, 0, 0, newYork),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setPollFlags(t, tt.window, tt.interval, true, false)

			if got := tt.now.Add(nextPollDelay(tt.now)); !got.Equal(tt.want) {
				t.Errorf("poll due %v, want %v", got, tt.want)
			}
		})
	}
}

// Whatever the window, interval and zone, a delay never lands in the window and
// never overshoots its end by more than the jitter.
// Not parallel: mutates package-level flag pointers.
func TestNextPollDelayNeverLandsInTheWindow(t *testing.T) {
	zones := make([]*time.Location, 0, 4)

	for _, name := range []string{"UTC", "Europe/Zagreb", "America/New_York", "Australia/Lord_Howe"} {
		zone, err := time.LoadLocation(name)
		if err != nil {
			t.Fatalf("LoadLocation(%q) = %v", name, err)
		}

		zones = append(zones, zone)
	}

	for range 20000 {
		q := quietWindow{start: rand.IntN(minutesPerDay), end: rand.IntN(minutesPerDay)} //nolint:gosec // test data
		if q.start == q.end {
			continue
		}

		interval := time.Duration(1+rand.IntN(72)) * time.Hour //nolint:gosec // test data
		jittered := rand.IntN(2) == 0                          //nolint:gosec // test data
		isDaemon := true

		origQuiet, origInterval, origJitter, origDaemon := quietHours, tickInterval, jitter, daemon
		quietHours, tickInterval, jitter, daemon = &q, &interval, &jittered, &isDaemon

		// Spring and autumn changes fall in March, April, October and November.
		month := []time.Month{time.January, time.March, time.April, time.July, time.October, time.November}[rand.IntN(6)] //nolint:gosec // test data
		now := time.Date(2026, month, 1+rand.IntN(28), rand.IntN(24), rand.IntN(60), 0, 0, zones[rand.IntN(len(zones))])  //nolint:gosec // test data

		delay := nextPollDelay(now)

		quietHours, tickInterval, jitter, daemon = origQuiet, origInterval, origJitter, origDaemon

		if due := now.Add(delay); delay <= 0 || q.contains(due) {
			t.Fatalf("window %v, interval %v: from %v due %v, inside the window", q.String(), interval, now, due)
		}

		if limit := 2*interval + 26*time.Hour; delay > limit {
			t.Fatalf("window %v, interval %v: from %v waits %v, past %v", q.String(), interval, now, delay, limit)
		}
	}
}
