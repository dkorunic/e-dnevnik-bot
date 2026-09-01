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

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
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

		// Spaced, not tight: POSIX does not queue duplicate pending signals, so
		// back-to-back SIGTERMs coalesce into one delivery and an unguarded
		// RequestShutdown would look identical to a guarded one. Spacing also
		// matches the real callers — LoggedOut and PairError arrive seconds
		// apart, which is when a surplus signal would cut the drain short.
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

// Exit codes for the PairError case, distinct from the shutdown ones above.
const (
	exitSessionKept    = 45
	exitSessionDeleted = 46
)

// TestPairErrorOnPairedClientKeepsSession: whatsmeow can deliver a spurious
// PairError to a client that is already paired and working. Treating every one
// as fatal deletes the session database, so a healthy install silently unpairs
// and the user must re-scan the QR code. Only an unpaired client (nil Store.ID)
// may take that path.
//
// Runs in a re-executed child: the unguarded path raises SIGTERM against the
// whole process, and RequestShutdown's sync.Once fires only once per process.
func TestPairErrorOnPairedClientKeepsSession(t *testing.T) {
	if os.Getenv(shutdownCaseEnv) == "paired-pair-error" {
		// Swallow the SIGTERM the unguarded path would raise, so the child
		// reports its verdict through an exit code instead of dying by signal.
		signal.Notify(make(chan os.Signal, 1), syscall.SIGTERM)

		dir := os.Getenv("EDNEVNIK_TEST_DIR")
		if err := os.Chdir(dir); err != nil {
			os.Exit(exitNoSignal)
		}

		// The handler removes WhatsAppDBName relative to the working directory.
		if err := os.WriteFile(WhatsAppDBName, []byte("session"), 0o600); err != nil {
			os.Exit(exitNoSignal)
		}

		// A paired client: Store.ID is set, so the PairError must be ignored.
		jid := types.JID{User: "12345", Server: types.DefaultUserServer}

		whatsAppCliMu.Lock()
		whatsAppCli = &whatsmeow.Client{Store: &store.Device{ID: &jid}}
		whatsAppCliMu.Unlock()

		whatsAppEventHandler(&events.PairError{})

		if _, err := os.Stat(WhatsAppDBName); err != nil {
			os.Exit(exitSessionDeleted)
		}

		os.Exit(exitSessionKept)
	}

	dir := t.TempDir()

	cmd := exec.Command(os.Args[0], "-test.run=TestPairErrorOnPairedClientKeepsSession") //nolint:gosec // re-exec of this test binary
	cmd.Env = append(os.Environ(), shutdownCaseEnv+"=paired-pair-error", "EDNEVNIK_TEST_DIR="+dir)

	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("child exited %v, want a verdict exit code\noutput:\n%s", err, out)
	}

	switch exitErr.ExitCode() {
	case exitSessionKept:
	case exitSessionDeleted:
		t.Fatalf("a PairError on an already-paired client deleted the session database; a healthy install would silently unpair and require a new QR scan\noutput:\n%s", out)
	default:
		t.Fatalf("child exited %d, want %d\noutput:\n%s", exitErr.ExitCode(), exitSessionKept, out)
	}
}
