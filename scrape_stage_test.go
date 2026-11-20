// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/scrape"
)

// No active classes must neither seed a user nor claim their digest week, yet
// it is no failure: over the summer break it is the normal answer every hour.
// Not parallel: swaps scrapeUser and the exitWithError latch.
func TestScrapersNoClassesIsNotACompleteScrape(t *testing.T) {
	origScrape := scrapeUser
	scrapeUser = func(context.Context, chan<- msgtypes.Message, string, string, uint) error {
		return scrape.ErrNoActiveClasses
	}

	origExit := exitWithError.Load()

	t.Cleanup(func() {
		scrapeUser = origScrape

		exitWithError.Store(origExit)
	})

	exitWithError.Store(false)

	r := uint(1)
	origRetries := retries
	retries = &r

	t.Cleanup(func() { retries = origRetries })

	var wg sync.WaitGroup

	failed := &userSet{}
	cfg := config.TomlConfig{User: []config.User{{Username: "a@skole.hr", Password: "p"}}}

	scrapers(t.Context(), &wg, make(chan msgtypes.Message, 1), cfg, failed)
	wg.Wait()

	if !failed.has("a@skole.hr") {
		t.Error("a scrape with no classes counts as complete, so it seeds the user and claims the digest week")
	}

	if exitWithError.Load() {
		t.Error("no active classes latched the run as failed")
	}
}

// A drifted scrape completed and its events went out, so it must still seed the
// user and claim the digest week; only sendDriftAlerts acts on it, and only
// once it repeats, so one cycle must not fail the run.
// Not parallel: swaps scrapeUser and the exitWithError latch.
func TestScrapersDriftIsACompleteScrape(t *testing.T) {
	origScrape := scrapeUser
	scrapeUser = func(context.Context, chan<- msgtypes.Message, string, string, uint) error {
		return &scrape.DriftError{Findings: []string{"prosjek ne odgovara ocjenama: Matematika"}}
	}

	origExit := exitWithError.Load()

	t.Cleanup(func() {
		scrapeUser = origScrape

		exitWithError.Store(origExit)
	})

	exitWithError.Store(false)

	r := uint(1)
	origRetries := retries
	retries = &r

	t.Cleanup(func() { retries = origRetries })

	var wg sync.WaitGroup

	failed := &userSet{}
	cfg := config.TomlConfig{User: []config.User{{Username: "a@skole.hr", Password: "p"}}}

	scrapers(t.Context(), &wg, make(chan msgtypes.Message, 1), cfg, failed)
	wg.Wait()

	if failed.has("a@skole.hr") {
		t.Error("a drifted scrape counts as failed, so it would never seed the user or send the digest")
	}

	if len(failed.driftOf("a@skole.hr")) != 1 {
		t.Error("drift findings not recorded for sendDriftAlerts")
	}

	if exitWithError.Load() {
		t.Error("a single drifted cycle latched the run as failed; only a confirmed drift should")
	}
}

// Without --driftcheck: a drifted scrape is an ordinary complete one, with no
// findings left for sendDriftAlerts.
// Not parallel: swaps scrapeUser and the driftCheck flag.
func TestScrapersDriftCheckOff(t *testing.T) {
	origScrape, origCheck := scrapeUser, driftCheck
	scrapeUser = func(context.Context, chan<- msgtypes.Message, string, string, uint) error {
		return &scrape.DriftError{Findings: []string{"prosjek ne odgovara ocjenama: Matematika"}}
	}

	off := false
	driftCheck = &off

	t.Cleanup(func() { scrapeUser, driftCheck = origScrape, origCheck })

	r := uint(1)
	origRetries := retries
	retries = &r

	t.Cleanup(func() { retries = origRetries })

	var wg sync.WaitGroup

	failed := &userSet{}
	cfg := config.TomlConfig{User: []config.User{{Username: "a@skole.hr", Password: "p"}}}

	scrapers(t.Context(), &wg, make(chan msgtypes.Message, 1), cfg, failed)
	wg.Wait()

	if failed.has("a@skole.hr") || len(failed.driftOf("a@skole.hr")) != 0 {
		t.Error("without --driftcheck a drifted scrape must count as complete and record no findings")
	}
}

// A scrape that skipped an optional page completed: the user is not failed (the
// digest and seed marker stand, the run does not latch), and the skipped code is
// recorded so its seed marker waits, alone or joined with drift.
// Not parallel: swaps scrapeUser, the exitWithError latch and --driftcheck.
func TestScrapersRecordsSkippedCodes(t *testing.T) {
	skipped := &scrape.SkippedError{Codes: []msgtypes.EventCode{msgtypes.Note}}

	for name, ret := range map[string]error{
		"alone":      skipped,
		"with drift": errors.Join(&scrape.DriftError{Findings: []string{"f"}}, skipped),
	} {
		t.Run(name, func(t *testing.T) {
			origScrape := scrapeUser
			scrapeUser = func(context.Context, chan<- msgtypes.Message, string, string, uint) error { return ret }

			origExit := exitWithError.Load()
			dc := true
			origDrift := driftCheck
			driftCheck = &dc

			r := uint(1)
			origRetries := retries
			retries = &r

			t.Cleanup(func() {
				scrapeUser = origScrape
				driftCheck = origDrift
				retries = origRetries

				exitWithError.Store(origExit)
			})

			exitWithError.Store(false)

			var wg sync.WaitGroup

			failed := &userSet{}
			cfg := config.TomlConfig{User: []config.User{{Username: "a@skole.hr", Password: "p"}}}

			scrapers(t.Context(), &wg, make(chan msgtypes.Message, 1), cfg, failed)
			wg.Wait()

			if failed.has("a@skole.hr") || exitWithError.Load() {
				t.Error("a scrape that only skipped a page counts as failed")
			}

			if !failed.skippedCode("a@skole.hr", msgtypes.Note) || failed.skippedCode("a@skole.hr", msgtypes.Absence) {
				t.Error("skipped codes not recorded as returned")
			}
		})
	}
}
