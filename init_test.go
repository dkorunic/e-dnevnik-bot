// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/appstate"
)

// A cancelled ctx must not end the wait: only done or the deadline may.
func TestAwaitShieldedIgnoresCancel(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	done := make(chan struct{})
	res := make(chan bool, 1)

	go func() { res <- awaitShielded(ctx, done, nil) }()

	select {
	case <-res:
		t.Fatal("awaitShielded returned on a cancelled ctx")
	case <-time.After(50 * time.Millisecond):
	}

	close(done)

	if !<-res {
		t.Fatal("awaitShielded reported a timeout after done fired")
	}
}

func TestAwaitShieldedDeadline(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	deadline := make(chan time.Time, 1)
	deadline <- time.Now()

	if awaitShielded(ctx, make(chan struct{}), deadline) {
		t.Fatal("awaitShielded reported done on deadline")
	}
}

// Not parallel: whatsAppPaired is package state.
func TestPairedMeanwhile(t *testing.T) {
	if pairedMeanwhile() {
		t.Fatal("pairedMeanwhile reported a pairing that never happened")
	}

	whatsAppPaired <- struct{}{}

	if !pairedMeanwhile() {
		t.Fatal("pairedMeanwhile missed a pending PairSuccess")
	}
}

// A second stop signal must end the wait, or only SIGKILL could.
// Not parallel: swaps the package-level notifyStop seam.
func TestAwaitShieldedSecondSignalAborts(t *testing.T) {
	subscribed := make(chan chan<- os.Signal, 1)

	orig := notifyStop
	notifyStop = func(c chan<- os.Signal) { subscribed <- c }

	t.Cleanup(func() { notifyStop = orig })

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res := make(chan bool, 1)

	go func() { res <- awaitShielded(ctx, make(chan struct{}), nil) }()

	// Subscribed only after the first signal (the cancel) was seen.
	c := <-subscribed
	c <- os.Interrupt

	select {
	case got := <-res:
		if got {
			t.Fatal("awaitShielded reported done after a second stop signal")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitShielded ignored the second stop signal")
	}
}

// Sync is complete only once every collection has finished or failed, not on
// the first one — critical_block lands well before the contact list.
// Not parallel: whatsAppPatchesDone is package state.
func TestMarkWhatsAppPatchDone(t *testing.T) {
	whatsAppPatchesMu.Lock()
	clear(whatsAppPatchesDone)
	whatsAppPatchesMu.Unlock()

	t.Cleanup(func() {
		whatsAppPatchesMu.Lock()
		clear(whatsAppPatchesDone)
		whatsAppPatchesMu.Unlock()
	})

	last := len(appstate.AllPatchNames) - 1

	for i, name := range appstate.AllPatchNames {
		// A repeat of an earlier collection must not count twice.
		if i > 0 && markWhatsAppPatchDone(appstate.AllPatchNames[0]) {
			t.Fatalf("reported complete after a repeat with %d of %d collections", i, last+1)
		}

		if got := markWhatsAppPatchDone(name); got != (i == last) {
			t.Fatalf("after %v (%d of %d): complete = %v", name, i+1, last+1, got)
		}
	}
}
