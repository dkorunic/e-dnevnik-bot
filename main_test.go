// @license
// Copyright (C) 2025 Dinko Korunic
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
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package main

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
)

// TestMain initialises package-level flag pointers that msgDedup and other
// functions dereference. Without this, any test that calls those functions
// would panic with a nil-pointer dereference.
func TestMain(m *testing.M) {
	dbg := false
	debugEvents = &dbg

	rl := false
	readingList = &rl

	rp := time.Duration(0)
	relevancePeriod = &rp

	os.Exit(m.Run())
}

func TestDurationRandJitter(t *testing.T) {
	t.Parallel()
	duration := 100 * time.Second
	min := time.Duration(float64(duration) * 0.9)
	max := time.Duration(float64(duration) * 1.1)

	for range 100 {
		jittered := durationRandJitter(duration)
		if jittered < min || jittered > max {
			t.Errorf("jittered duration %v is outside the expected range [%v, %v]", jittered, min, max)
		}
	}
}

// openExistingDB is a test helper that creates a DB, seeds one entry so the
// file exists on disk, closes it, and re-opens it. On the second open
// Existing() returns true, simulating a "not first run" scenario.
func openExistingDB(t *testing.T, path string) *sqlitedb.Edb {
	t.Helper()

	eDB, err := sqlitedb.New(context.Background(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New failed: %v", err)
	}

	_, _ = eDB.CheckAndFlagTTL(context.Background(), "seed", "seed", []string{"seed"})
	eDB.Close()

	eDB, err = sqlitedb.New(context.Background(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New (reopen) failed: %v", err)
	}

	return eDB
}

// TestMsgDedupYearInferenceFutureMonth verifies Bug 8A from TESTING-PLAN:
// a grade whose DD.MM. date has a month GREATER than the current month must be
// assigned last year. Swapping the two AddDate branches would assign it the
// current year, making it appear to be a future date and passing the filter.
func TestMsgDedupYearInferenceFutureMonth(t *testing.T) {
	// Cannot run in parallel — modifies package-level flag pointers.
	now := time.Now()

	// Skip in December: adding 1 wraps to January, making year ambiguous.
	if now.Month() == time.December {
		t.Skip("skipping in December: month-wrap makes year ambiguous")
	}

	futureMonth := now.Month() + 1
	dateStr := time.Date(0, futureMonth, 1, 0, 0, 0, 0, time.UTC).Format("2.1.")

	// Set relevance period short enough (30 days) to suppress last year's grade.
	rp := 30 * 24 * time.Hour
	relevancePeriod = &rp

	defer func() {
		zero := time.Duration(0)
		relevancePeriod = &zero
	}()

	eDB := openExistingDB(t, t.TempDir()+"/dedup-future.db")
	defer eDB.Close()

	gradesScraped := make(chan msgtypes.Message, 1)
	gradesMsg := make(chan msgtypes.Message, 10)

	gradesScraped <- msgtypes.Message{
		Code:     msgtypes.Grade,
		Username: "testuser",
		Subject:  "Matematika",
		Fields:   []string{dateStr, "5"},
	}
	close(gradesScraped)

	var wg sync.WaitGroup

	msgDedup(context.Background(), eDB, &wg, gradesScraped, gradesMsg)
	wg.Wait()

	// A grade from a future month should be treated as last year (>30 days old)
	// and therefore suppressed by the relevance filter.
	if len(gradesMsg) > 0 {
		t.Errorf("grade from future month %v should be suppressed as last year's grade (Bug 8A: swapped AddDate branches)", futureMonth)
	}
}

// TestMsgDedupYearInferenceSameDayNotSuppressed verifies Bug 8B from TESTING-PLAN:
// a grade received TODAY (same month and day) must be treated as this year and
// NOT suppressed. The `>=` off-by-one mutation would treat today's grade as
// last year's, causing the relevance filter to suppress a valid alert.
func TestMsgDedupYearInferenceSameDayNotSuppressed(t *testing.T) {
	// Cannot run in parallel — modifies package-level flag pointers.
	now := time.Now()
	todayStr := time.Date(0, now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).Format("2.1.")

	// Use a 1-day relevance period: last-year assignment would fail this filter,
	// but current-year assignment (today) must pass.
	rp := 24 * time.Hour
	relevancePeriod = &rp

	defer func() {
		zero := time.Duration(0)
		relevancePeriod = &zero
	}()

	eDB := openExistingDB(t, t.TempDir()+"/dedup-today.db")
	defer eDB.Close()

	gradesScraped := make(chan msgtypes.Message, 1)
	gradesMsg := make(chan msgtypes.Message, 10)

	gradesScraped <- msgtypes.Message{
		Code:     msgtypes.Grade,
		Username: "testuser",
		Subject:  "Fizika",
		Fields:   []string{todayStr, "5"},
	}
	close(gradesScraped)

	var wg sync.WaitGroup

	msgDedup(context.Background(), eDB, &wg, gradesScraped, gradesMsg)
	wg.Wait()

	// msgDedup closes gradesMsg via defer; drain it after Wait.
	var msgs []msgtypes.Message
	for m := range gradesMsg {
		msgs = append(msgs, m)
	}

	if len(msgs) == 0 {
		t.Error("today's grade should NOT be suppressed (Bug 8B: >= instead of > treats today as last year)")
	}
}
