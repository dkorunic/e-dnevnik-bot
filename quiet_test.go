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

func at(hour, minute int) time.Time {
	return time.Date(2026, time.November, 2, hour, minute, 30, 0, time.UTC)
}

func TestQuietWindowContains(t *testing.T) {
	tests := []struct {
		name   string
		window string
		now    time.Time
		want   bool
	}{
		{"before a wrapping window", "23:00-06:00", at(22, 59), false},
		{"start is inclusive", "23:00-06:00", at(23, 0), true},
		{"before midnight", "23:00-06:00", at(23, 59), true},
		{"after midnight", "23:00-06:00", at(0, 0), true},
		{"last quiet minute", "23:00-06:00", at(5, 59), true},
		{"end is exclusive", "23:00-06:00", at(6, 0), false},
		{"midday is not quiet", "23:00-06:00", at(12, 0), false},
		{"before a same-day window", "13:15-14:45", at(13, 14), false},
		{"inside a same-day window", "13:15-14:45", at(14, 44), true},
		{"after a same-day window", "13:15-14:45", at(14, 45), false},
		{"midnight end wraps", "22:00-00:00", at(23, 30), true},
		{"midnight end stops at midnight", "22:00-00:00", at(0, 0), false},
		{"off disables", "off", at(23, 30), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var q quietWindow
			if err := q.Set(tt.window); err != nil {
				t.Fatalf("Set(%q) = %v", tt.window, err)
			}

			if got := q.contains(tt.now); got != tt.want {
				t.Errorf("%q contains %v = %v, want %v", tt.window, tt.now.Format("15:04"), got, tt.want)
			}
		})
	}
}

func TestQuietWindowSetRejects(t *testing.T) {
	for _, in := range []string{
		"", // ff cannot deliver --quiethours= anyway: it reads as a missing value
		"nonsense",
		"23:00",
		"23:00-",
		"-06:00",
		"24:00-06:00",
		"23:60-06:00",
		"23-06",
		"23:00-06:00-07:00",
		"06:00-06:00", // whole day or never: refuse to guess
	} {
		t.Run(in, func(t *testing.T) {
			q := quietWindow{start: 1, end: 2}
			if err := q.Set(in); err == nil {
				t.Errorf("Set(%q) accepted, parsed as %q", in, q.String())
			}
		})
	}
}

// A one-shot run was asked for explicitly: cron and CI decide when those fire.
func TestPollSkipped(t *testing.T) {
	night := quietWindow{start: 23 * 60, end: 6 * 60}

	orig := quietHours
	quietHours = &night

	t.Cleanup(func() { quietHours = orig })

	tests := []struct {
		name   string
		now    time.Time
		daemon bool
		want   bool
	}{
		{"daemon inside the window", at(3, 0), true, true},
		{"daemon outside the window", at(12, 0), true, false},
		{"one-shot inside the window", at(3, 0), false, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setBoolFlag(t, &daemon, tt.daemon)

			if got := pollSkipped(tt.now); got != tt.want {
				t.Errorf("pollSkipped = %v, want %v", got, tt.want)
			}
		})
	}
}

// A typo must stop startup like a bad -i does, not quietly poll all night.
func TestParseFlagsRejectsBadQuietHours(t *testing.T) {
	if args := os.Getenv(startupCaseEnv); strings.HasPrefix(args, "parseflags ") {
		os.Args = append([]string{"e-dnevnik-bot"}, strings.Fields(strings.TrimPrefix(args, "parseflags "))...)

		parseFlags()

		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestParseFlagsRejectsBadQuietHours") //nolint:gosec // re-exec of this test binary
	cmd.Env = append(os.Environ(), startupCaseEnv+"=parseflags --quiethours=25:00-06:00")

	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("invalid --quiethours exited cleanly, want non-zero\noutput:\n%s", out)
	}

	// Any crash exits non-zero too; only this text proves the value was rejected.
	if !strings.Contains(string(out), errQuietFormat.Error()) {
		t.Errorf("invalid --quiethours failed for another reason:\n%s", out)
	}
}
