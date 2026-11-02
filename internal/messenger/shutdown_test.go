// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// shutdownCaseEnv names the case a re-executed child process should run.
const shutdownCaseEnv = "EDNEVNIK_MESSENGER_SHUTDOWN_CASE"

// exitNoSignal reports a child that could not set up its case.
const exitNoSignal = 43

// RequestShutdown has to work without signals: Windows supports only Kill through
// Process.Signal, so a self-signalled SIGTERM there did nothing, silently.
// Not parallel: swaps the registered shutdown function.
func TestRequestShutdownRunsRegisteredFunc(t *testing.T) {
	t.Cleanup(func() { SetShutdown(nil) })

	calls := 0

	SetShutdown(func() { calls++ })
	RequestShutdown()

	if calls == 0 {
		t.Error("RequestShutdown did not run the registered function")
	}
}

// Fatal events can arrive before main has registered anything, and in tests.
// Not parallel: swaps the registered shutdown function.
func TestRequestShutdownWithoutRegistrationIsSafe(t *testing.T) {
	SetShutdown(nil)
	RequestShutdown()
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
// Runs in a re-executed child: it changes directory and swaps the client global.
func TestPairErrorOnPairedClientKeepsSession(t *testing.T) {
	if os.Getenv(shutdownCaseEnv) == "paired-pair-error" {
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

// A new database handle opens every poll cycle, so an entry nobody forgets pins a
// closed handle for each failing cycle of a daemon's life.
func TestForgetSendFailuresDropsOnlyThatDatabase(t *testing.T) {
	t.Parallel()

	finished, running := &sqlitedb.Edb{}, &sqlitedb.Edb{}

	recordSendFailure(finished, MailQueueName, nil)
	recordSendFailure(running, MailQueueName, nil)

	ForgetSendFailures(finished)

	if err := sendFailureErr(finished, MailQueueName); err != nil {
		t.Errorf("sendFailureErr(finished) = %v, want nil once forgotten", err)
	}

	if err := sendFailureErr(running, MailQueueName); err == nil {
		t.Error("forgetting one database also forgot another's failures")
	}
}
