// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package logger

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// fatalCaseEnv names the exit case a re-executed child process should run.
const fatalCaseEnv = "EDNEVNIK_LOGGER_FATAL_CASE"

// TestFatalExitsNonZero covers logger.Fatal, which cannot be tested in-process:
// zerolog calls os.Exit(1) from inside Msg. The exit is the whole contract, and
// it is why msgDedup deliberately does *not* use Fatal on a database error —
// os.Exit skips deferred cleanup and in-flight messenger queue writes.
//
// The message must also reach the output before the exit; a process that dies
// silently leaves an operator with no reason for the failure.
func TestFatalExitsNonZero(t *testing.T) {
	if os.Getenv(fatalCaseEnv) == "fatal" {
		Fatal().Msg("fatal-marker")

		// Unreachable: Msg must have exited. Exit 0 so the parent reports it.
		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestFatalExitsNonZero") //nolint:gosec // re-exec of this test binary
	cmd.Env = append(os.Environ(), fatalCaseEnv+"=fatal")

	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("Fatal().Msg() did not exit the process (err %v); callers rely on it being terminal\noutput:\n%s", err, out)
	}

	if got := exitErr.ExitCode(); got != 1 {
		t.Errorf("exit code = %d, want 1", got)
	}

	if !bytes.Contains(out, []byte("fatal-marker")) {
		t.Errorf("the fatal message was not written before exiting; operators would get no reason for the failure\noutput:\n%s", out)
	}
}

// TestFatalSkipsDeferredFunctions pins the distinction that drives the design
// decision in msgDedup: Fatal exits via os.Exit, so deferred cleanup never
// runs. If this ever stopped being true, the SIGTERM-to-self workaround there
// would be unnecessary.
func TestFatalSkipsDeferredFunctions(t *testing.T) {
	if os.Getenv(fatalCaseEnv) == "defer" {
		defer func() {
			// Must never run. If it does, the parent sees the marker.
			Info().Msg("deferred-ran")
		}()

		Fatal().Msg("exiting-now")

		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestFatalSkipsDeferredFunctions") //nolint:gosec // re-exec of this test binary
	cmd.Env = append(os.Environ(), fatalCaseEnv+"=defer")

	out, _ := cmd.CombinedOutput()

	if bytes.Contains(out, []byte("deferred-ran")) {
		t.Errorf("a deferred function ran after Fatal; os.Exit must skip defers\noutput:\n%s", out)
	}
}

// TestPanicPanics: unlike Fatal, Panic unwinds the stack normally, so deferred
// functions and recover both work. A caller can therefore contain it, which is
// what makes the messengers' panic guards viable.
// Not parallel: swaps the package-level Logger, which the parallel tests read.
func TestPanicPanics(t *testing.T) {
	var buf bytes.Buffer

	orig := Logger

	t.Cleanup(func() { Logger = orig })

	Logger = Logger.Output(&buf)

	deferRan := false

	func() {
		defer func() {
			deferRan = true

			r := recover()
			if r == nil {
				t.Error("Panic().Msg() did not panic; the messengers' recover guards would never fire")

				return
			}

			if msg, ok := r.(string); ok && !strings.Contains(msg, "panic-marker") {
				t.Errorf("panic value = %q, want it to carry the message", msg)
			}
		}()

		Panic().Msg("panic-marker")
	}()

	if !deferRan {
		t.Error("the deferred function did not run; Panic must unwind rather than exit")
	}

	if !strings.Contains(buf.String(), "panic-marker") {
		t.Errorf("the panic message was not logged: %q", buf.String())
	}
}
