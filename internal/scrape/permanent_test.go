// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/fetch"
)

// TestMarkPermanentClassification pins which fetch errors short-circuit the
// retry loop. Getting this wrong is expensive in both directions: retrying bad
// credentials re-submits the same POST and re-trips the portal's rate limiter,
// while marking a transient network fault permanent abandons a scrape that
// would have succeeded on the next attempt.
func TestMarkPermanentClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		err           error
		wantPermanent bool
	}{
		{
			name:          "nil stays nil",
			err:           nil,
			wantPermanent: false,
		},
		{
			name:          "invalid login is permanent",
			err:           fetch.ErrInvalidLogin,
			wantPermanent: true,
		},
		{
			name:          "wrapped invalid login is permanent",
			err:           fmt.Errorf("login step: %w", fetch.ErrInvalidLogin),
			wantPermanent: true,
		},
		{
			name:          "oversized body is permanent",
			err:           fetch.ErrBodyTooLarge,
			wantPermanent: true,
		},
		{
			name:          "wrapped oversized body is permanent",
			err:           fmt.Errorf("fetching grades: %w", fetch.ErrBodyTooLarge),
			wantPermanent: true,
		},
		{
			name:          "unexpected status is transient",
			err:           fetch.ErrUnexpectedStatus,
			wantPermanent: false,
		},
		{
			name:          "missing CSRF token is transient",
			err:           fetch.ErrCSRFToken,
			wantPermanent: false,
		},
		{
			name:          "invalid class ID is transient",
			err:           fetch.ErrInvalidClassID,
			wantPermanent: false,
		},
		{
			name:          "network timeout is transient",
			err:           &net.OpError{Op: "dial", Err: errors.New("i/o timeout")},
			wantPermanent: false,
		},
		{
			name:          "context cancellation is transient",
			err:           context.Canceled,
			wantPermanent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := markPermanent(tt.err)

			if tt.err == nil {
				if got != nil {
					t.Fatalf("markPermanent(nil) = %v, want nil", got)
				}

				return
			}

			// The original error must remain inspectable through the wrapper,
			// or callers lose the ability to distinguish failure causes.
			if !errors.Is(got, tt.err) {
				t.Errorf("markPermanent(%v) = %v, which no longer unwraps to the original error", tt.err, got)
			}

			if gotPermanent := retry.IsRecoverable(got) == false; gotPermanent != tt.wantPermanent {
				t.Errorf("markPermanent(%v) permanent = %v, want %v", tt.err, gotPermanent, tt.wantPermanent)
			}
		})
	}
}

// TestMarkPermanentStopsRetryLoop verifies the classification has the intended
// effect on retry-go rather than only setting a flag: a permanent error must
// consume exactly one attempt, a transient one all of them.
func TestMarkPermanentStopsRetryLoop(t *testing.T) {
	t.Parallel()

	const attempts = 4

	run := func(err error) int {
		calls := 0

		_ = retry.New(
			retry.Attempts(attempts),
			retry.Delay(0),
			retry.MaxDelay(0),
			retry.Context(context.Background()),
		).Do(func() error {
			calls++

			return markPermanent(err)
		})

		return calls
	}

	if got := run(fetch.ErrInvalidLogin); got != 1 {
		t.Errorf("invalid login made %d attempts, want 1 — retrying re-trips the portal rate limiter", got)
	}

	if got := run(fetch.ErrBodyTooLarge); got != 1 {
		t.Errorf("oversized body made %d attempts, want 1 — the condition is deterministic", got)
	}

	if got := run(fetch.ErrUnexpectedStatus); got != attempts {
		t.Errorf("unexpected status made %d attempts, want %d — a 5xx may well succeed on retry", got, attempts)
	}
}
