// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func setPollFlags(t *testing.T, window string, interval time.Duration, daemonMode, jittered bool) {
	t.Helper()

	var q quietWindow
	if err := q.Set(window); err != nil {
		t.Fatalf("Set(%q) = %v", window, err)
	}

	origQuiet, origInterval := quietHours, tickInterval
	quietHours, tickInterval = &q, &interval

	t.Cleanup(func() { quietHours, tickInterval = origQuiet, origInterval })

	setBoolFlag(t, &daemon, daemonMode)
	setBoolFlag(t, &jitter, jittered)
}

// A delay that lands inside the window must wait for its end. Re-arming a full
// interval instead kept `-i 24h` started at 03:00 in the window forever.
func TestNextPollDelay(t *testing.T) {
	tests := []struct {
		name     string
		window   string
		now      time.Time
		interval time.Duration
		daemon   bool
		want     time.Duration
	}{
		{"landing outside keeps the interval", "23:00-06:00", at(12, 0), 6 * time.Hour, true, 6 * time.Hour},
		{"landing inside waits for the end", "23:00-06:00", at(23, 50), 6 * time.Hour, true, 6*time.Hour + 9*time.Minute + 30*time.Second},
		{"daily interval inside the window cannot starve", "23:00-06:00", at(3, 0), 24 * time.Hour, true, 26*time.Hour + 59*time.Minute + 30*time.Second},
		{"off keeps the interval", "off", at(3, 0), 24 * time.Hour, true, 24 * time.Hour},
		{"one-shot keeps the interval", "23:00-06:00", at(3, 0), 24 * time.Hour, false, 24 * time.Hour},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setPollFlags(t, tt.window, tt.interval, tt.daemon, false)

			if got := nextPollDelay(tt.now); got != tt.want {
				t.Errorf("nextPollDelay = %v, want %v", got, tt.want)
			}
		})
	}
}

// The wake-up jitter must stay inside the active gap: past it, the tick lands
// in the next window and a near-all-day window never polls at all.
func TestNextPollDelayJitterStaysInActiveGap(t *testing.T) {
	setPollFlags(t, "00:00-23:59", time.Hour, true, true)

	now := at(12, 0)

	for range 500 {
		if due := now.Add(nextPollDelay(now)); quietHours.contains(due) {
			t.Fatalf("poll due %v is still inside the window", due)
		}
	}
}

// The end is a wall-clock time: the spring-forward hour must not move it.
func TestNextPollDelayAcrossDST(t *testing.T) {
	zagreb, err := time.LoadLocation("Europe/Zagreb")
	if err != nil {
		t.Fatalf("LoadLocation() = %v", err)
	}

	setPollFlags(t, "23:00-06:00", 6*time.Hour, true, false)

	// Six real hours end at 05:30, an hour later on the wall than without DST.
	now := time.Date(2026, time.March, 28, 22, 30, 0, 0, zagreb)
	due := now.Add(nextPollDelay(now)).In(zagreb)

	if got := due.Format("15:04"); got != "06:00" {
		t.Errorf("poll due at %v, want 06:00", got)
	}
}

// The first poll validates credentials and scraping, so it must run even
// inside the window: a bad password should not wait for morning.
func TestPollRuns(t *testing.T) {
	setPollFlags(t, "23:00-06:00", time.Hour, true, false)

	if !pollRuns(at(3, 0), true) {
		t.Error("first poll skipped inside the window")
	}

	if pollRuns(at(3, 0), false) {
		t.Error("later poll ran inside the window")
	}

	if !pollRuns(at(12, 0), false) {
		t.Error("later poll skipped outside the window")
	}
}

func TestQuietWindowActiveSpan(t *testing.T) {
	tests := []struct {
		window string
		want   time.Duration
	}{
		{"23:00-06:00", 17 * time.Hour},
		{"13:00-14:00", 23 * time.Hour},
		{"00:00-23:59", time.Minute},
		{"off", 24 * time.Hour},
	}

	for _, tt := range tests {
		var q quietWindow
		if err := q.Set(tt.window); err != nil {
			t.Fatalf("Set(%q) = %v", tt.window, err)
		}

		if got := q.activeSpan(); got != tt.want {
			t.Errorf("%q activeSpan = %v, want %v", tt.window, got, tt.want)
		}
	}
}

// The zone is the only sign of a container silently running the window in UTC.
func TestQuietWindowDescribe(t *testing.T) {
	var q quietWindow
	if err := q.Set("23:00-06:00"); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	if got, want := q.describe(at(12, 0)), "23:00-06:00 UTC"; got != want {
		t.Errorf("describe = %q, want %q", got, want)
	}

	if err := q.Set("off"); err != nil {
		t.Fatalf("Set() = %v", err)
	}

	if got, want := q.describe(at(12, 0)), "off"; got != want {
		t.Errorf("describe = %q, want %q", got, want)
	}
}

func TestQuietHoursHelpHasNoBackticks(t *testing.T) {
	if args := os.Getenv(startupCaseEnv); args == "parseflags-help" {
		os.Args = []string{"e-dnevnik-bot", "--help"}

		parseFlags()

		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestQuietHoursHelpHasNoBackticks") //nolint:gosec // re-exec of this test binary
	cmd.Env = append(os.Environ(), startupCaseEnv+"=parseflags-help")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("--help exited with %v\noutput:\n%s", err, out)
	}

	if !strings.Contains(string(out), "--quiethours HH:MM-HH:MM") {
		t.Errorf("--help lost the placeholder:\n%s", out)
	}

	if strings.Contains(string(out), "`") {
		t.Errorf("--help prints literal backticks:\n%s", out)
	}
}
