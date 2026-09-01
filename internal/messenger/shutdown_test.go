// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// shutdownCaseEnv names the case a re-executed child process should run.
const shutdownCaseEnv = "EDNEVNIK_MESSENGER_SHUTDOWN_CASE"

// Distinct exit codes let the parent tell apart "signal arrived", "signal never
// arrived" and "a second signal arrived".
const (
	exitSignalReceived = 42
	exitNoSignal       = 43
	exitExtraSignal    = 44
)

// TestRequestShutdownSignalsSelf covers the SIGTERM-to-self mechanism that
// replaces logger.Fatal for unrecoverable messenger conditions. Fatal's
// os.Exit would bypass queue persistence and deferred cleanup; signalling
// instead routes through main's signal.NotifyContext and runs the normal
// graceful shutdown, draining the failed-message queue on the way out.
//
// It runs in a re-executed child because it signals the whole process and
// because its sync.Once can only fire once per process lifetime.
func TestRequestShutdownSignalsSelf(t *testing.T) {
	if os.Getenv(shutdownCaseEnv) == "signal" {
		sigCh := make(chan os.Signal, 2)
		signal.Notify(sigCh, syscall.SIGTERM)

		RequestShutdown()

		select {
		case <-sigCh:
		case <-time.After(30 * time.Second):
			os.Exit(exitNoSignal)
		}

		os.Exit(exitSignalReceived)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRequestShutdownSignalsSelf") //nolint:gosec // re-exec of this test binary
	cmd.Env = append(os.Environ(), shutdownCaseEnv+"=signal")

	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child exited %v, want the distinctive signal-received code\noutput:\n%s", err, out)
	}

	switch exitErr.ExitCode() {
	case exitSignalReceived:
	case exitNoSignal:
		t.Fatalf("RequestShutdown did not deliver SIGTERM; an unrecoverable messenger error would never trigger graceful shutdown\noutput:\n%s", out)
	default:
		t.Fatalf("child exited %d, want %d\noutput:\n%s", exitErr.ExitCode(), exitSignalReceived, out)
	}
}

// TestRequestShutdownFiresOnce pins the sync.Once. Repeated unrecoverable
// events (say, every recipient failing in one cycle) must not each raise a
// fresh SIGTERM: a second signal arriving mid-drain would cut the graceful
// shutdown short and lose the queue writes it was in the middle of.
func TestRequestShutdownFiresOnce(t *testing.T) {
	if os.Getenv(shutdownCaseEnv) == "once" {
		sigCh := make(chan os.Signal, 4)
		signal.Notify(sigCh, syscall.SIGTERM)

		// Spaced, not a tight loop: POSIX does not queue duplicate pending
		// signals, so five SIGTERMs raised back-to-back coalesce into one
		// delivery and an unguarded RequestShutdown would look identical to a
		// guarded one. Spacing them past the handler lets each surplus signal
		// actually land. This also matches how the real callers behave — the
		// WhatsApp events that trigger a shutdown (LoggedOut, PairError) arrive
		// seconds apart, which is exactly when a second SIGTERM would cut the
		// graceful drain short.
		for range 5 {
			RequestShutdown()
			time.Sleep(150 * time.Millisecond)
		}

		select {
		case <-sigCh:
		case <-time.After(30 * time.Second):
			os.Exit(exitNoSignal)
		}

		// Give any surplus signals a chance to land before concluding.
		select {
		case <-sigCh:
			os.Exit(exitExtraSignal)
		case <-time.After(2 * time.Second):
		}

		os.Exit(exitSignalReceived)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestRequestShutdownFiresOnce") //nolint:gosec // re-exec of this test binary
	cmd.Env = append(os.Environ(), shutdownCaseEnv+"=once")

	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child exited %v, want the distinctive signal-received code\noutput:\n%s", err, out)
	}

	switch exitErr.ExitCode() {
	case exitSignalReceived:
	case exitExtraSignal:
		t.Fatalf("RequestShutdown raised more than one SIGTERM; a second signal would cut the graceful drain short\noutput:\n%s", out)
	case exitNoSignal:
		t.Fatalf("RequestShutdown delivered no SIGTERM\noutput:\n%s", out)
	default:
		t.Fatalf("child exited %d, want %d\noutput:\n%s", exitErr.ExitCode(), exitSignalReceived, out)
	}
}
