// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package sqlitedb

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

// TestCheckAndFlagTTLIsExclusivePerKey: for one key, exactly one concurrent
// caller may see found=false and none may error. That is what stops two scrapers
// racing on the same event and both forwarding it as new.
//
// Scope, so this is not mistaken for more than it is: it does NOT detect a
// BEGIN IMMEDIATE -> BEGIN downgrade. Measured at 64 goroutines x 20 runs, a
// deferred-begin build produced no double-flag — WAL, a four-connection pool and
// a very short critical section serialise the calls anyway. That write lock is a
// review-time invariant. What this does catch is losing the transaction outright.
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
