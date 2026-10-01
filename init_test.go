// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"testing"
	"time"
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
