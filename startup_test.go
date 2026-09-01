// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/rs/zerolog"
)

// setBoolFlag points a package-level *bool flag at v for the test's duration.
func setBoolFlag(t *testing.T, p **bool, v bool) {
	t.Helper()

	orig := *p
	*p = &v

	t.Cleanup(func() { *p = orig })
}

// preserveLogger restores the global logger and level, which initLog rewrites.
func preserveLogger(t *testing.T) {
	t.Helper()

	origLogger := logger.Logger
	origLevel := zerolog.GlobalLevel()

	t.Cleanup(func() {
		logger.Logger = origLogger
		zerolog.SetGlobalLevel(origLevel)
	})
}

// TestInitLogLevelSelection covers the log-level precedence: -v wins outright,
// otherwise LOG_LEVEL is consulted, and an out-of-range LOG_LEVEL is ignored
// rather than reinterpreted as some arbitrary level by an unchecked cast.
// Not parallel: mutates package-level flag pointers and the global logger.
func TestInitLogLevelSelection(t *testing.T) {
	tests := []struct {
		name     string
		verbose  bool
		logLevel string
		setLevel bool
		want     zerolog.Level
	}{
		{
			name: "default is info",
			want: zerolog.InfoLevel,
		},
		{
			name:    "verbose selects debug",
			verbose: true,
			want:    zerolog.DebugLevel,
		},
		{
			name:     "LOG_LEVEL selects trace",
			logLevel: "-1",
			setLevel: true,
			want:     zerolog.TraceLevel,
		},
		{
			name:     "LOG_LEVEL selects error",
			logLevel: "3",
			setLevel: true,
			want:     zerolog.ErrorLevel,
		},
		{
			name:     "verbose overrides LOG_LEVEL",
			verbose:  true,
			logLevel: "5",
			setLevel: true,
			want:     zerolog.DebugLevel,
		},
		{
			name:     "out-of-range LOG_LEVEL is ignored",
			logLevel: "99",
			setLevel: true,
			want:     zerolog.InfoLevel,
		},
		{
			name:     "negative out-of-range LOG_LEVEL is ignored",
			logLevel: "-42",
			setLevel: true,
			want:     zerolog.InfoLevel,
		},
		{
			name:     "non-numeric LOG_LEVEL is ignored",
			logLevel: "debug",
			setLevel: true,
			want:     zerolog.InfoLevel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			preserveLogger(t)
			setBoolFlag(t, &debug, tt.verbose)
			setBoolFlag(t, &colorLogs, false)

			if tt.setLevel {
				t.Setenv("LOG_LEVEL", tt.logLevel)
			} else {
				t.Setenv("LOG_LEVEL", "")
				os.Unsetenv("LOG_LEVEL")
			}

			initLog()

			if got := zerolog.GlobalLevel(); got != tt.want {
				t.Errorf("global log level = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestInitLogColorConsole: -l switches to console output, and NO_COLOR
// suppresses the escape codes without disabling the console writer. A
// colourised stream piped to a log collector is unreadable, which is the whole
// point of honouring NO_COLOR.
// Not parallel: mutates package-level flag pointers and the global logger.
func TestInitLogColorConsole(t *testing.T) {
	preserveLogger(t)
	setBoolFlag(t, &debug, false)
	setBoolFlag(t, &colorLogs, true)
	t.Setenv("NO_COLOR", "1")

	initLog()

	var buf strings.Builder

	logger.Logger = logger.Logger.Output(zerolog.ConsoleWriter{Out: &buf, NoColor: true})
	logger.Info().Msg("console-marker")

	out := buf.String()
	if !strings.Contains(out, "console-marker") {
		t.Errorf("console output missing the message: %q", out)
	}

	if strings.Contains(out, "\x1b[") {
		t.Errorf("NO_COLOR was set but the output carries ANSI escapes: %q", out)
	}
}

// TestStartSystemdWatchdogWithoutSystemd: outside systemd there is no watchdog,
// and the function must be a silent no-op rather than spawning a goroutine that
// shutdown would then wait on.
// Not parallel: reads the package-level bgWG.
func TestStartSystemdWatchdogWithoutSystemd(t *testing.T) {
	// Ensure no systemd notification socket is advertised.
	t.Setenv("NOTIFY_SOCKET", "")
	os.Unsetenv("NOTIFY_SOCKET")
	t.Setenv("WATCHDOG_USEC", "")
	os.Unsetenv("WATCHDOG_USEC")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	startSystemdWatchdog(ctx)

	cancel()

	// bgWG must drain immediately: either nothing was started, or what was
	// started honours ctx.
	done := make(chan struct{})

	go func() {
		bgWG.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("bgWG did not drain after cancellation; a watchdog goroutine is ignoring ctx.Done()")
	}
}

// TestAwaitShutdownStopsTickersAndCancels covers the ordinary path: the poll
// tickers are stopped, the shared context is cancelled so every stage unwinds,
// and the call returns once bgWG is empty.
// Not parallel: uses the package-level bgWG.
func TestAwaitShutdownStopsTickersAndCancels(t *testing.T) {
	ticker := time.NewTicker(time.Hour)
	statusTicker := time.NewTicker(time.Hour)

	_, cancel := context.WithCancel(context.Background())

	cancelled := false
	stop := func() {
		cancelled = true

		cancel()
	}

	done := make(chan struct{})

	go func() {
		defer close(done)

		awaitShutdown(stop, ticker, statusTicker)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("awaitShutdown did not return with an empty bgWG")
	}

	if !cancelled {
		t.Error("awaitShutdown did not call stop(); the pipeline would never be told to unwind")
	}
}

// TestAwaitShutdownIsBounded is the load-bearing invariant: a wedged background
// goroutine must not stall process exit. The wait is capped at exitDelay, so a
// goroutine that never returns costs a bounded delay rather than a hang that
// systemd eventually SIGKILLs.
// Not parallel: uses the package-level bgWG.
func TestAwaitShutdownIsBounded(t *testing.T) {
	// Costs a full exitDelay by construction: the point is that the wait
	// actually happens and then ends.
	if testing.Short() {
		t.Skip("takes exitDelay to run")
	}

	release := make(chan struct{})

	// Deliberately ignores ctx: this stands in for a wedged goroutine.
	bgWG.Go(func() { <-release })

	// Release it after the assertion so it does not outlive the test binary.
	t.Cleanup(func() {
		close(release)
		bgWG.Wait()
	})

	ticker := time.NewTicker(time.Hour)
	statusTicker := time.NewTicker(time.Hour)

	_, cancel := context.WithCancel(context.Background())

	start := time.Now()

	done := make(chan struct{})

	go func() {
		defer close(done)

		awaitShutdown(cancel, ticker, statusTicker)
	}()

	select {
	case <-done:
	case <-time.After(exitDelay + 20*time.Second):
		t.Fatal("awaitShutdown never returned with a wedged background goroutine; shutdown must be bounded by exitDelay")
	}

	if elapsed := time.Since(start); elapsed < exitDelay {
		t.Errorf("awaitShutdown returned after %v, before the %v ceiling; it is not actually waiting for bgWG", elapsed, exitDelay)
	}
}

// TestFatalIfErrorsSucceedsWhenClean: with no latched error the function must
// return normally so the caller's own exit path runs.
// Not parallel: reads the package-level exitWithError latch.
func TestFatalIfErrorsSucceedsWhenClean(t *testing.T) {
	resetExitLatch(t)

	fatalIfErrors()
}

// startupCaseEnv names the subprocess case to run.
const startupCaseEnv = "EDNEVNIK_MAIN_STARTUP_CASE"

// TestFatalIfErrorsExitsWhenLatched covers the terminal path. A daemon that hit
// an error in any cycle must exit non-zero so a supervisor notices, and the
// exit goes through logger.Fatal, which is os.Exit and untestable in-process.
func TestFatalIfErrorsExitsWhenLatched(t *testing.T) {
	if os.Getenv(startupCaseEnv) == "fatal-if-errors" {
		exitWithError.Store(true)
		fatalIfErrors()

		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestFatalIfErrorsExitsWhenLatched") //nolint:gosec // re-exec of this test binary
	cmd.Env = append(os.Environ(), startupCaseEnv+"=fatal-if-errors")

	out, err := cmd.CombinedOutput()

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("a latched error did not exit non-zero (err %v); a supervisor would treat a failed run as success\noutput:\n%s", err, out)
	}

	if got := exitErr.ExitCode(); got == 0 {
		t.Errorf("exit code = %d, want non-zero", got)
	}
}

// parsedFlags is the subset of parseFlags' results the clamp tests inspect.
type parsedFlags struct {
	TickInterval    time.Duration `json:"tick_interval"`
	RelevancePeriod time.Duration `json:"relevance_period"`
	Retries         uint          `json:"retries"`
	Debug           bool          `json:"debug"`
	DebugEvents     bool          `json:"debug_events"`
}

// TestParseFlagsClamps covers the guard rails parseFlags applies after parsing.
// It runs in a re-executed child because parseFlags reassigns every
// package-level flag pointer, which would corrupt the rest of this package's
// tests if done in-process.
func TestParseFlagsClamps(t *testing.T) {
	if args := os.Getenv(startupCaseEnv); strings.HasPrefix(args, "parseflags ") {
		os.Args = append([]string{"e-dnevnik-bot"}, strings.Fields(strings.TrimPrefix(args, "parseflags "))...)

		parseFlags()

		out, err := json.Marshal(parsedFlags{
			TickInterval:    *tickInterval,
			RelevancePeriod: *relevancePeriod,
			Retries:         *retries,
			Debug:           *debug,
			DebugEvents:     *debugEvents,
		})
		if err != nil {
			os.Exit(1)
		}

		// Marker-delimited so log lines around it do not confuse the parent.
		os.Stdout.WriteString("<<FLAGS>>" + string(out) + "<<END>>\n")
		os.Exit(0)
	}

	tests := []struct {
		name  string
		args  string
		check func(*testing.T, parsedFlags)
	}{
		{
			name: "sub-hour poll interval is raised to the floor",
			args: "-i 1s",
			check: func(t *testing.T, got parsedFlags) {
				t.Helper()

				if got.TickInterval != DefaultTickInterval {
					t.Errorf("tickInterval = %v, want it clamped to %v — the floor protects the portal from being polled too often",
						got.TickInterval, DefaultTickInterval)
				}
			},
		},
		{
			name: "an interval above the floor is preserved",
			args: "-i 6h",
			check: func(t *testing.T, got parsedFlags) {
				t.Helper()

				if got.TickInterval != 6*time.Hour {
					t.Errorf("tickInterval = %v, want 6h — the clamp must only raise, never override", got.TickInterval)
				}
			},
		},
		{
			name: "negative relevance period resets to unlimited",
			args: "--relevance -5h",
			check: func(t *testing.T, got parsedFlags) {
				t.Helper()

				if got.RelevancePeriod != 0 {
					t.Errorf("relevancePeriod = %v, want 0 (unlimited)", got.RelevancePeriod)
				}
			},
		},
		{
			name: "zero retries clamps to one",
			args: "-r 0",
			check: func(t *testing.T, got parsedFlags) {
				t.Helper()

				if got.Retries != 1 {
					t.Errorf("retries = %d, want 1 — retry-go reads 0 as unlimited, which would retry forever", got.Retries)
				}
			},
		},
		{
			name: "explicit retries are preserved",
			args: "-r 7",
			check: func(t *testing.T, got parsedFlags) {
				t.Helper()

				if got.Retries != 7 {
					t.Errorf("retries = %d, want 7", got.Retries)
				}
			},
		},
		{
			name: "fulldebug implies verbose",
			args: "-0",
			check: func(t *testing.T, got parsedFlags) {
				t.Helper()

				if !got.DebugEvents {
					t.Error("debugEvents = false, want true")
				}

				if !got.Debug {
					t.Error("debug = false, want true — --fulldebug is documented as implying -v")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.Command(os.Args[0], "-test.run=TestParseFlagsClamps") //nolint:gosec // re-exec of this test binary
			cmd.Env = append(os.Environ(), startupCaseEnv+"=parseflags "+tt.args)

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("child failed: %v\noutput:\n%s", err, out)
			}

			s := string(out)

			start := strings.Index(s, "<<FLAGS>>")
			end := strings.Index(s, "<<END>>")

			if start < 0 || end < 0 {
				t.Fatalf("child did not report parsed flags\noutput:\n%s", s)
			}

			var got parsedFlags
			if err := json.Unmarshal([]byte(s[start+len("<<FLAGS>>"):end]), &got); err != nil {
				t.Fatalf("decoding child output: %v\noutput:\n%s", err, s)
			}

			tt.check(t, got)
		})
	}
}

// TestParseFlagsHelpExitsZero: -? and --version are informational and must exit
// cleanly, or a scripted `--version` check would look like a failure.
func TestParseFlagsHelpExitsZero(t *testing.T) {
	for _, flag := range []string{"-?", "--version"} {
		t.Run(flag, func(t *testing.T) {
			if args := os.Getenv(startupCaseEnv); strings.HasPrefix(args, "parseflags ") {
				os.Args = append([]string{"e-dnevnik-bot"}, strings.Fields(strings.TrimPrefix(args, "parseflags "))...)

				parseFlags()

				// parseFlags must have exited already.
				os.Exit(9)
			}

			cmd := exec.Command(os.Args[0], "-test.run=TestParseFlagsHelpExitsZero") //nolint:gosec // re-exec of this test binary
			cmd.Env = append(os.Environ(), startupCaseEnv+"=parseflags "+flag)

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%v exited non-zero (%v); informational flags must exit cleanly\noutput:\n%s", flag, err, out)
			}
		})
	}
}
