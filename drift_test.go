// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

func TestWeekMonday(t *testing.T) {
	t.Parallel()

	for _, day := range []int{9, 12, 15} { // Monday, Thursday, Sunday
		now := time.Date(2026, time.November, day, 23, 30, 0, 0, time.Local)
		if got, want := weekMonday(now), time.Date(2026, time.November, 9, 0, 0, 0, 0, time.Local); !got.Equal(want) {
			t.Errorf("weekMonday(%v) = %v, want %v", now, got, want)
		}
	}
}

// TestRecordDrift walks the state machine: one sighting only arms, a repeat
// confirms and alerts once per week, and a clean scrape disarms it but keeps
// this week's alert on record.
func TestRecordDrift(t *testing.T) {
	t.Parallel()

	eDB := openExistingDB(t, filepath.Join(t.TempDir(), "drift.db"))
	defer eDB.Close() //nolint:errcheck

	week1 := time.Date(2026, time.November, 9, 0, 0, 0, 0, time.UTC)
	week2 := week1.AddDate(0, 0, 7)
	week3 := week1.AddDate(0, 0, 14)
	week4 := week1.AddDate(0, 0, 21)

	for i, step := range []struct {
		drifted          bool
		monday           time.Time
		confirmed, alert bool
	}{
		{false, week1, false, false},
		{true, week1, false, false}, // first sighting
		{true, week1, true, true},   // confirmed: alert
		{true, week1, true, false},  // already alerted this week
		{true, week2, true, true},   // a new week alerts again
		{false, week2, false, false},
		{true, week2, false, false}, // a clean scrape disarms: one sighting again
		{true, week2, true, false},  // flapping: still only one alert this week
		{false, week2, false, false},
		{true, week3, false, false}, // an old alert must not skip the first sighting
		{true, week3, true, true},   // confirmed in a new week: alert
		{false, week4, false, false},
		{true, week4, false, false}, // a clean scrape in a later week forgets it all
		{false, week4, false, false},
	} {
		confirmed, alert, err := recordDrift(t.Context(), eDB, "pero", step.drifted, step.monday)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}

		if confirmed != step.confirmed || alert != step.alert {
			t.Errorf("step %d: confirmed=%v alert=%v, want %v %v", i, confirmed, alert, step.confirmed, step.alert)
		}
	}
}

// TestSendDriftAlerts: a failed scrape is no verdict, so it neither alerts nor
// resets the armed state; the next drifted scrape confirms.
// Not parallel: exitWithError is process-global.
func TestSendDriftAlerts(t *testing.T) {
	origExit := exitWithError.Load()
	t.Cleanup(func() { exitWithError.Store(origExit) })

	exitWithError.Store(false)

	eDB := openExistingDB(t, filepath.Join(t.TempDir(), "drift-send.db"))
	defer eDB.Close() //nolint:errcheck

	now := time.Date(2026, time.November, 12, 10, 0, 0, 0, time.UTC)
	users := []string{"pero"}
	findings := []string{"prosjek ne odgovara ocjenama: Matematika"}

	run := func(set *userSet) []msgtypes.Message {
		ch := make(chan msgtypes.Message, 4)
		sendDriftAlerts(t.Context(), eDB, &seedRun{failed: set, users: users}, ch, now)
		close(ch)

		var out []msgtypes.Message
		for m := range ch {
			out = append(out, m)
		}

		return out
	}

	drifted := &userSet{}
	drifted.addDrift("pero", findings)

	failed := &userSet{}
	failed.add("pero")

	if got := run(drifted); len(got) != 0 {
		t.Fatalf("first sighting alerted: %+v", got)
	}

	if got := run(failed); len(got) != 0 {
		t.Fatalf("failed scrape alerted: %+v", got)
	}

	got := run(drifted)
	if len(got) != 1 {
		t.Fatalf("confirmed drift sent %d alerts, want 1", len(got))
	}

	if m := got[0]; m.Code != msgtypes.PortalDrift || m.Username != "pero" || m.Fields[1] != findings[0] ||
		!m.Timestamp.Equal(weekMonday(now)) {
		t.Errorf("alert = %+v", m)
	}

	if !exitWithError.Load() {
		t.Error("confirmed drift did not fail the run")
	}
}
