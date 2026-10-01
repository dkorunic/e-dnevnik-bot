// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// zagreb provides real DST transitions for window arithmetic.
func zagreb(t *testing.T) *time.Location {
	t.Helper()

	loc, err := time.LoadLocation("Europe/Zagreb")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}

	return loc
}

// TestDigestWeek pins the send window from day@hour until the covered Monday.
func TestDigestWeek(t *testing.T) {
	t.Parallel()

	loc := zagreb(t)
	at := func(y int, m time.Month, d, h, mi int) time.Time { return time.Date(y, m, d, h, mi, 0, 0, loc) }

	tests := []struct {
		name       string
		now        time.Time
		day        time.Weekday
		hour       int
		wantMonday string
		wantDue    bool
	}{
		{"sunday before hour", at(2026, 10, 4, 17, 59), time.Sunday, 18, "2026-10-05", false},
		{"sunday at hour", at(2026, 10, 4, 18, 0), time.Sunday, 18, "2026-10-05", true},
		{"sunday late", at(2026, 10, 4, 23, 59), time.Sunday, 18, "2026-10-05", true},
		{"monday rolls to next week", at(2026, 10, 5, 0, 5), time.Sunday, 18, "2026-10-12", false},
		{"saturday not yet", at(2026, 10, 3, 20, 0), time.Sunday, 18, "2026-10-05", false},
		{"friday config on friday", at(2026, 10, 2, 9, 0), time.Friday, 8, "2026-10-05", true},
		{"friday config catches up saturday", at(2026, 10, 3, 1, 0), time.Friday, 8, "2026-10-05", true},
		{"friday config on thursday", at(2026, 10, 1, 23, 0), time.Friday, 8, "2026-10-05", false},
		{"monday config waits for hour", at(2026, 10, 5, 7, 0), time.Monday, 8, "2026-10-12", false},
		{"monday config due after hour", at(2026, 10, 5, 8, 0), time.Monday, 8, "2026-10-12", true},
		{"month and year boundary", at(2026, 12, 27, 18, 30), time.Sunday, 18, "2026-12-28", true},
		{"DST ends overnight", at(2026, 10, 25, 18, 0), time.Sunday, 18, "2026-10-26", true},
		{"midnight hour", at(2026, 10, 4, 0, 0), time.Sunday, 0, "2026-10-05", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			monday, due := digestWeek(tt.now, tt.day, tt.hour)

			if got := monday.Format(time.DateOnly); got != tt.wantMonday || due != tt.wantDue {
				t.Errorf("digestWeek(%v, %v, %d) = %v, %v; want %v, %v",
					tt.now, tt.day, tt.hour, got, due, tt.wantMonday, tt.wantDue)
			}

			if monday.Weekday() != time.Monday || monday.Hour() != 0 {
				t.Errorf("digestWeek returned %v, want local midnight on a Monday", monday)
			}
		})
	}
}

// examOn builds an exam shaped like scrape.parseEvents output.
func examOn(user, subject, date, note string) msgtypes.Message {
	ts, _ := time.Parse(time.DateOnly, date)

	return msgtypes.Message{
		Code:         msgtypes.Exam,
		Username:     user,
		Subject:      subject,
		Descriptions: []string{"Predmet", "Datum ispita", "Napomena"},
		Fields:       []string{subject, ts.Format("02.01.2006."), note},
		Timestamp:    ts,
	}
}

// TestBuildDigest checks Monday–Sunday selection, sorting, dedup and note formatting.
func TestBuildDigest(t *testing.T) {
	t.Parallel()

	monday := time.Date(2026, 10, 5, 0, 0, 0, 0, zagreb(t))

	exams := []msgtypes.Message{
		examOn("u", "Fizika", "2026-10-09", ""),
		examOn("u", "Matematika", "2026-10-05", "pisana provjera"),
		examOn("u", "Biologija", "2026-10-09", "usmeno"),
		examOn("u", "Kemija", "2026-10-04", "prošli tjedan"),
		examOn("u", "Povijest", "2026-10-12", "sljedeći tjedan"),
		examOn("u", "Glazbeni", "2026-10-11", "nedjelja"),
		examOn("u", "Matematika", "2026-10-05", "pisana provjera"),
	}

	got, ok := buildDigest("u", exams, monday)
	if !ok {
		t.Fatal("buildDigest reported an empty week")
	}

	if got.Code != msgtypes.ExamDigest || got.Username != "u" || got.Subject != "5.10. – 11.10." {
		t.Errorf("header = %v/%q/%q, want ExamDigest/u/\"5.10. – 11.10.\"", got.Code, got.Username, got.Subject)
	}

	wantDesc := []string{"pon 5.10.", "pet 9.10.", "pet 9.10.", "ned 11.10."}
	wantFields := []string{"Matematika – pisana provjera", "Biologija – usmeno", "Fizika", "Glazbeni – nedjelja"}

	if !slices.Equal(got.Descriptions, wantDesc) || !slices.Equal(got.Fields, wantFields) {
		t.Errorf("rows = %q / %q\nwant %q / %q", got.Descriptions, got.Fields, wantDesc, wantFields)
	}
}

// TestBuildDigestEmptyWeek: an empty week produces no message.
func TestBuildDigestEmptyWeek(t *testing.T) {
	t.Parallel()

	monday := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)

	if _, ok := buildDigest("u", []msgtypes.Message{examOn("u", "Kemija", "2026-10-04", "")}, monday); ok {
		t.Error("buildDigest produced a message for a week with no exams")
	}

	if _, ok := buildDigest("u", nil, monday); ok {
		t.Error("buildDigest produced a message with no exams at all")
	}
}

// TestBuildDigestShortLegacyFields: short Fields must not index out of range.
func TestBuildDigestShortLegacyFields(t *testing.T) {
	t.Parallel()

	monday := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	g := examOn("u", "Fizika", "2026-10-06", "")
	g.Fields = nil

	got, ok := buildDigest("u", []msgtypes.Message{g}, monday)
	if !ok || !slices.Equal(got.Fields, []string{"Fizika"}) {
		t.Errorf("buildDigest = %+v, %v; want one Fizika row", got, ok)
	}
}

// runDigestCycle runs msgDedup with dg and returns only the emitted digests.
func runDigestCycle(t *testing.T, eDB *sqlitedb.Edb, dg *digestRun, msgs ...msgtypes.Message) []msgtypes.Message {
	t.Helper()

	gradesScraped := make(chan msgtypes.Message, len(msgs)+1)
	for _, m := range msgs {
		gradesScraped <- m
	}

	close(gradesScraped)

	gradesMsg := make(chan msgtypes.Message, len(msgs)+len(dg.users)+1)

	var wg sync.WaitGroup

	msgDedup(t.Context(), eDB, &wg, gradesScraped, gradesMsg, dg)
	wg.Wait()

	var out []msgtypes.Message

	for m := range gradesMsg {
		if m.Code == msgtypes.ExamDigest {
			out = append(out, m)
		}
	}

	return out
}

// sundayEvening falls inside the default window for the week of 5.10.2026.
var sundayEvening = time.Date(2026, 10, 4, 19, 0, 0, 0, time.UTC)

// TestDigestSentOncePerWeek: one digest per window, another next week.
// Not parallel: mutates package-level flag pointers.
func TestDigestSentOncePerWeek(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	eDB := openExistingDB(t, filepath.Join(t.TempDir(), "digest-once.db"))
	defer eDB.Close() //nolint:errcheck

	dg := &digestRun{now: sundayEvening, users: []string{"u"}, day: time.Sunday, hour: 18}
	exams := []msgtypes.Message{examOn("u", "Matematika", "2026-10-06", ""), examOn("u", "Fizika", "2026-10-14", "")}

	got := runDigestCycle(t, eDB, dg, exams...)
	if len(got) != 1 || !slices.Equal(got[0].Fields, []string{"Matematika"}) {
		t.Fatalf("first cycle sent %+v, want one digest listing Matematika", got)
	}

	dg.now = sundayEvening.Add(time.Hour)
	if got := runDigestCycle(t, eDB, dg, exams...); len(got) != 0 {
		t.Fatalf("second cycle in the same window sent %d digests, want 0", len(got))
	}

	dg.now = sundayEvening.AddDate(0, 0, 7)

	got = runDigestCycle(t, eDB, dg, exams...)
	if len(got) != 1 || !slices.Equal(got[0].Fields, []string{"Fizika"}) {
		t.Fatalf("next week's cycle sent %+v, want one digest listing Fizika", got)
	}
}

// TestDigestOutsideWindowSendsNothing: an early cycle must not claim the week.
// Not parallel: mutates package-level flag pointers.
func TestDigestOutsideWindowSendsNothing(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	eDB := openExistingDB(t, filepath.Join(t.TempDir(), "digest-early.db"))
	defer eDB.Close() //nolint:errcheck

	exam := examOn("u", "Matematika", "2026-10-06", "")
	dg := &digestRun{now: sundayEvening.Add(-2 * time.Hour), users: []string{"u"}, day: time.Sunday, hour: 18}

	if got := runDigestCycle(t, eDB, dg, exam); len(got) != 0 {
		t.Fatalf("sent %d digests before the window opened, want 0", len(got))
	}

	dg.now = sundayEvening
	if got := runDigestCycle(t, eDB, dg, exam); len(got) != 1 {
		t.Fatalf("sent %d digests once the window opened, want 1", len(got))
	}
}

// TestDigestSkipsFailedScrape: a failed user stays unclaimed and retries next cycle.
// Not parallel: mutates package-level flag pointers.
func TestDigestSkipsFailedScrape(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	eDB := openExistingDB(t, filepath.Join(t.TempDir(), "digest-failed.db"))
	defer eDB.Close() //nolint:errcheck

	failed := &userSet{}
	failed.add("a")

	exams := []msgtypes.Message{examOn("a", "Matematika", "2026-10-06", ""), examOn("b", "Fizika", "2026-10-07", "")}
	dg := &digestRun{now: sundayEvening, users: []string{"a", "b"}, day: time.Sunday, hour: 18, failed: failed}

	got := runDigestCycle(t, eDB, dg, exams...)
	if len(got) != 1 || got[0].Username != "b" {
		t.Fatalf("sent %+v, want only b's digest while a's scrape failed", got)
	}

	dg.failed = &userSet{}
	dg.now = sundayEvening.Add(time.Hour)

	got = runDigestCycle(t, eDB, dg, exams...)
	if len(got) != 1 || got[0].Username != "a" {
		t.Fatalf("recovery cycle sent %+v, want only a's postponed digest", got)
	}
}

// TestDigestEmptyWeekIsClaimedSilently: no exams sends nothing but claims the week.
// Not parallel: mutates package-level flag pointers.
func TestDigestEmptyWeekIsClaimedSilently(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	eDB := openExistingDB(t, filepath.Join(t.TempDir(), "digest-empty.db"))
	defer eDB.Close() //nolint:errcheck

	dg := &digestRun{now: sundayEvening, users: []string{"u"}, day: time.Sunday, hour: 18}

	if got := runDigestCycle(t, eDB, dg); len(got) != 0 {
		t.Fatalf("sent %d digests for an empty week, want 0", len(got))
	}

	claimed, err := claimDigestWeek(t.Context(), eDB, "u", time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if err != nil || claimed {
		t.Errorf("claimDigestWeek after an empty week = %v, %v; want already claimed", claimed, err)
	}
}

// TestDigestOnFirstRun: the first-run seed suppresses alerts, not the digest.
// Not parallel: mutates package-level flag pointers.
func TestDigestOnFirstRun(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), "digest-fresh.db"))
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}
	defer eDB.Close() //nolint:errcheck

	dg := &digestRun{now: sundayEvening, users: []string{"u"}, day: time.Sunday, hour: 18}

	gradesScraped := make(chan msgtypes.Message, 1)
	gradesScraped <- examOn("u", "Matematika", "2026-10-06", "")
	close(gradesScraped)

	gradesMsg := make(chan msgtypes.Message, 4)

	var wg sync.WaitGroup

	msgDedup(t.Context(), eDB, &wg, gradesScraped, gradesMsg, dg)
	wg.Wait()

	var codes []msgtypes.EventCode
	for m := range gradesMsg {
		codes = append(codes, m.Code)
	}

	if !slices.Equal(codes, []msgtypes.EventCode{msgtypes.ExamDigest}) {
		t.Errorf("first run forwarded %v, want only the digest", codes)
	}
}

// TestDigestPanicDoesNotForwardSkippedEvent: a skipped last event must not be
// resent by recoverDedup when sendDigests panics.
// Not parallel: mutates package-level flag pointers and the sendDigestsFn seam.
func TestDigestPanicDoesNotForwardSkippedEvent(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)
	resetExitLatch(t)

	eDB := openExistingDB(t, filepath.Join(t.TempDir(), "digest-panic.db"))
	defer eDB.Close() //nolint:errcheck

	// Flag it once, so the next cycle skips it as a duplicate.
	if got := runDedup(t, t.Context(), eDB, grade("Matematika")); len(got) != 1 {
		t.Fatalf("seeding cycle forwarded %d alerts, want 1", len(got))
	}

	orig := sendDigestsFn
	sendDigestsFn = func(context.Context, *sqlitedb.Edb, *digestRun, map[string][]msgtypes.Message, chan<- msgtypes.Message) {
		panic("injected digest failure")
	}

	t.Cleanup(func() { sendDigestsFn = orig })

	dg := &digestRun{now: sundayEvening, users: []string{"testuser"}, day: time.Sunday, hour: 18}

	gradesScraped := make(chan msgtypes.Message, 1)
	gradesScraped <- grade("Matematika")
	close(gradesScraped)

	gradesMsg := make(chan msgtypes.Message, 2)

	var wg sync.WaitGroup

	msgDedup(t.Context(), eDB, &wg, gradesScraped, gradesMsg, dg)
	wg.Wait()

	for m := range gradesMsg {
		t.Errorf("forwarded %v/%v after a digest panic; a skipped duplicate must stay skipped", m.Username, m.Subject)
	}
}
