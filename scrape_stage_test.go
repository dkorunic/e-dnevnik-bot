// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
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
