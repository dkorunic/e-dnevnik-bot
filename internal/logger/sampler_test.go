// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package logger

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

// TestSample verifies the sampler wrapper actually attaches the sampler rather
// than returning the bare logger. A no-op wrapper would silently disable rate
// limiting on any hot log path that adopts it.
func TestSample(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	// BasicSampler(2) keeps every 2nd event.
	l := Sample(&zerolog.BasicSampler{N: 2}).Output(&buf)

	const total = 10

	for range total {
		l.Info().Msg("sampled")
	}

	got := strings.Count(buf.String(), "sampled")
	if got == total {
		t.Fatalf("all %d events were emitted; Sample() did not attach the sampler", total)
	}

	if got == 0 {
		t.Fatal("no events were emitted; Sample() dropped everything")
	}
}

// TestSampleNeverSampler pins the other extreme: a sampler that rejects
// everything must actually suppress output through the wrapper.
func TestSampleNeverSampler(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	l := Sample(zerolog.LevelSampler{InfoSampler: &zerolog.BasicSampler{N: 0}}).Output(&buf)
	l.Info().Msg("suppressed")

	if strings.Contains(buf.String(), "suppressed") {
		t.Error("an event passed a reject-all sampler; Sample() is not wiring the sampler through")
	}
}

// countingHook records how many events it saw and stamps each one, so the test
// can assert both that the hook runs and that its fields reach the output.
type countingHook struct {
	calls int
}

func (h *countingHook) Run(e *zerolog.Event, _ zerolog.Level, _ string) {
	h.calls++

	e.Str("hooked", "yes")
}

// TestHook verifies the hook wrapper attaches the hook and that fields the hook
// adds land in the emitted event.
func TestHook(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	h := &countingHook{}
	l := Hook(h).Output(&buf)

	l.Info().Msg("first")
	l.Warn().Msg("second")

	if h.calls != 2 {
		t.Errorf("hook ran %d times, want 2", h.calls)
	}

	out := buf.String()
	if !strings.Contains(out, "hooked") {
		t.Errorf("hook-added field missing from output: %q", out)
	}

	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Errorf("hooked logger dropped messages: %q", out)
	}
}

// TestHookDoesNotMutateGlobalLogger: both wrappers return a *derived* logger.
// If they mutated the package-level Logger instead, one caller attaching a
// sampler would silently rate-limit every other caller in the process.
func TestHookDoesNotMutateGlobalLogger(t *testing.T) {
	var buf bytes.Buffer

	origLogger := Logger

	t.Cleanup(func() { Logger = origLogger })

	Logger = Logger.Output(&buf)

	h := &countingHook{}
	_ = Hook(h)
	_ = Sample(&zerolog.BasicSampler{N: 100})

	Logger.Info().Msg("direct")

	if h.calls != 0 {
		t.Error("the global logger ran a hook attached to a derived logger; Hook() must not mutate Logger")
	}

	if !strings.Contains(buf.String(), "direct") {
		t.Error("the global logger stopped emitting after Sample(); Sample() must not mutate Logger")
	}
}
