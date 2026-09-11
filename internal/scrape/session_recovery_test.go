// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package scrape

import (
	"errors"
	"testing"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/fetch"
)

// TestRecoverSessionBoundsConsecutiveRecoveries pins the ceiling on the in-place
// recovery loop: a step that never recovers costs exactly maxSessionRecoveries
// logins, then stops unrecoverably.
func TestRecoverSessionBoundsConsecutiveRecoveries(t *testing.T) {
	t.Parallel()

	var logins int

	budget := maxSessionRecoveries

	err := recoverSession(
		func() error { return fetch.ErrSessionExpired },
		func() error { logins++; return nil },
		&budget, "user@skole.hr",
	)

	if logins != maxSessionRecoveries {
		t.Errorf("logins = %d, want exactly %d before giving up", logins, maxSessionRecoveries)
	}

	if !errors.Is(err, fetch.ErrSessionExpired) || !isUnrecoverable(err) {
		t.Errorf("err = %v, want an unrecoverable ErrSessionExpired", err)
	}

	if budget != 0 {
		t.Errorf("budget = %d, want it fully spent", budget)
	}
}

// TestRecoverSessionStopsWhenBudgetExhausted is the point of the budget: a
// drifted marker makes every endpoint report expiry, and unbounded recovery
// answers with a login POST per attempt per step.
func TestRecoverSessionStopsWhenBudgetExhausted(t *testing.T) {
	t.Parallel()

	var logins int

	budget := 0

	err := recoverSession(
		func() error { return fetch.ErrSessionExpired },
		func() error { logins++; return nil },
		&budget, "user@skole.hr",
	)

	if logins != 0 {
		t.Errorf("logins = %d, want 0 once the budget is spent", logins)
	}

	if !errors.Is(err, retry.Unrecoverable(fetch.ErrSessionExpired)) && !isUnrecoverable(err) {
		t.Errorf("err = %v, want an unrecoverable error so retry-go stops", err)
	}
}

// TestRecoverSessionPassesThroughOtherErrors: unrelated failures must not spend
// login budget.
func TestRecoverSessionPassesThroughOtherErrors(t *testing.T) {
	t.Parallel()

	var logins int

	budget := maxSessionRecoveries
	sentinel := errors.New("connection reset")

	err := recoverSession(
		func() error { return sentinel },
		func() error { logins++; return nil },
		&budget, "user@skole.hr",
	)

	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want the original error untouched", err)
	}

	if logins != 0 {
		t.Errorf("logins = %d, want 0 for a non-session error", logins)
	}

	if budget != maxSessionRecoveries {
		t.Errorf("budget = %d, want it untouched", budget)
	}
}

// TestRecoverSessionMarksLoginFailurePermanent keeps bad credentials out of the
// portal's rate limiter.
func TestRecoverSessionMarksLoginFailurePermanent(t *testing.T) {
	t.Parallel()

	budget := maxSessionRecoveries

	err := recoverSession(
		func() error { return fetch.ErrSessionExpired },
		func() error { return fetch.ErrInvalidLogin },
		&budget, "user@skole.hr",
	)

	if !errors.Is(err, fetch.ErrInvalidLogin) {
		t.Errorf("err = %v, want ErrInvalidLogin", err)
	}

	if !isUnrecoverable(err) {
		t.Errorf("err = %v, want it unrecoverable so the cycle stops", err)
	}
}

// isUnrecoverable reports whether retry-go would short-circuit on err.
func isUnrecoverable(err error) bool {
	return !retry.IsRecoverable(err)
}

// TestRecoverSessionResetsBudgetAfterProgress separates the two cases a flat
// ceiling conflates: drift fails consecutively and must stop, while a long
// scrape (-r up to 100) legitimately outlives several sessions. A step that
// succeeds proves the session live.
func TestRecoverSessionResetsBudgetAfterProgress(t *testing.T) {
	t.Parallel()

	budget := 0

	if err := recoverSession(
		func() error { return nil },
		func() error { t.Fatal("login called for a successful step"); return nil },
		&budget, "user@skole.hr",
	); err != nil {
		t.Fatalf("recoverSession = %v, want nil", err)
	}

	if budget != maxSessionRecoveries {
		t.Errorf("budget = %d, want it restored to %d after a step succeeded", budget, maxSessionRecoveries)
	}
}

// TestRecoverSessionRetriesAfterSuccessfulLogin covers the attempt with no
// successor. Delegating the re-run to retry-go discards the recovery when the
// failing attempt was the last — and -r 1 makes every attempt the last.
func TestRecoverSessionRetriesAfterSuccessfulLogin(t *testing.T) {
	t.Parallel()

	var calls, logins int

	budget := maxSessionRecoveries

	err := recoverSession(
		func() error {
			calls++
			if calls == 1 {
				return fetch.ErrSessionExpired
			}

			return nil
		},
		func() error { logins++; return nil },
		&budget, "user@skole.hr",
	)
	if err != nil {
		t.Fatalf("recoverSession = %v, want nil — the re-authenticated step succeeded", err)
	}

	if calls != 2 {
		t.Errorf("step ran %d times, want 2: once failing, once against the fresh session", calls)
	}

	if logins != 1 {
		t.Errorf("logins = %d, want 1", logins)
	}
}

// TestMarkPermanentStopsOnAuthMarkerDrift keeps the property splitting the
// sentinel out of ErrInvalidLogin could have lost: a better diagnosis, not a
// softer one. Retrying resolves neither cause it covers.
func TestMarkPermanentStopsOnAuthMarkerDrift(t *testing.T) {
	t.Parallel()

	err := markPermanent(fetch.ErrAuthMarkerMissing)

	if !errors.Is(err, fetch.ErrAuthMarkerMissing) {
		t.Fatalf("markPermanent = %v, want it to preserve ErrAuthMarkerMissing", err)
	}

	if !isUnrecoverable(err) {
		t.Error("markPermanent left auth-marker drift retryable; retrying cannot resolve it")
	}
}
