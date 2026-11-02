// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/messenger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// RequestShutdown has to end the same context the signals do.
// Not parallel: registers the process-wide shutdown function.
func TestSignalContextEndsOnRequestShutdown(t *testing.T) {
	ctx, stop := signalContext()
	defer stop()

	t.Cleanup(func() { messenger.SetShutdown(nil) })

	messenger.RequestShutdown()

	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Error("RequestShutdown did not end the main context")
	}
}

// A broken database stops dedup, but scrapers still sending must be able to
// finish whatever the shutdown request does: a scraper parked on a full channel
// holds wgScrape.Wait forever.
// Not parallel: registers the process-wide shutdown function and uses the latch.
func TestMsgDedupDatabaseFailureDoesNotWedgeScrapers(t *testing.T) {
	resetExitLatch(t)
	setRelevancePeriod(t, 0)
	setReadingList(t, false)

	requested := make(chan struct{}, 1)
	messenger.SetShutdown(func() {
		select {
		case requested <- struct{}{}:
		default:
		}
	})

	t.Cleanup(func() { messenger.SetShutdown(nil) })

	eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), "broken.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	_ = eDB.Close()

	// Unbuffered, and a context nothing cancels: the worst case for the sender.
	scraped := make(chan msgtypes.Message)
	sent := make(chan struct{})

	go func() {
		defer close(sent)
		defer close(scraped)

		for range 3 {
			scraped <- grade("event")
		}
	}()

	var wg sync.WaitGroup

	msgDedup(t.Context(), eDB, &wg, scraped, make(chan msgtypes.Message, 3), nil, nil)

	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("a scraper stayed blocked after dedup stopped")
	}

	wg.Wait()

	select {
	case <-requested:
	default:
		t.Error("a broken database did not request a shutdown")
	}

	if !exitWithError.Load() {
		t.Error("a broken database did not latch exitWithError")
	}
}
