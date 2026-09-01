// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package sqlitedb

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// TestCheckAndFlagTTLIsExclusivePerKey pins first-run exclusivity: for a single
// key, exactly one concurrent caller may observe found=false, and none may
// error. That property is what stops two scraper goroutines racing on the same
// event and both forwarding it as new — a duplicate alert.
//
// Scope note, so this test is not mistaken for more than it is: it does **not**
// reliably detect a downgrade of the flag transaction from BEGIN IMMEDIATE to
// BEGIN. That was measured — 64 goroutines x 20 runs against a deferred-begin
// build produced no double-flag and no error, because WAL, a four-connection
// pool and a very short critical section make the calls serialise on their own.
// The immediate write lock is defensive hardening whose absence this access
// pattern does not currently expose; keeping it is a review-time invariant, not
// something a black-box test can enforce.
//
// What this test does catch is a real loss of per-key exclusivity: dropping the
// transaction altogether, or a keying bug that lets two callers flag under
// different keys.
func TestCheckAndFlagTTLIsExclusivePerKey(t *testing.T) {
	t.Parallel()

	const goroutines = 16

	eDB, err := New(context.Background(), filepath.Join(t.TempDir(), "exclusive.db"))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	var (
		start     = make(chan struct{})
		wg        sync.WaitGroup
		mu        sync.Mutex
		newCount  int
		errsFound []error
	)

	for range goroutines {
		wg.Add(1)

		go func() {
			defer wg.Done()

			<-start

			found, err := eDB.CheckAndFlagTTL(context.Background(), "pero.peric", "Matematika", []string{"5", "odlican"})

			mu.Lock()
			defer mu.Unlock()

			if err != nil {
				errsFound = append(errsFound, err)

				return
			}

			if !found {
				newCount++
			}
		}()
	}

	close(start)
	wg.Wait()

	if len(errsFound) > 0 {
		t.Errorf("%d of %d concurrent CheckAndFlagTTL calls errored (first: %v); contention must serialise, not surface as an error to the caller",
			len(errsFound), goroutines, errsFound[0])
	}

	if newCount != 1 {
		t.Errorf("%d of %d concurrent callers saw the key as new, want exactly 1; per-key exclusivity is what stops two goroutines both alerting on the same event",
			newCount, goroutines)
	}
}
