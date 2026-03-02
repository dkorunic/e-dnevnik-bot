package logger

import (
	"bytes"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestOutput(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := Output(&buf)
	logger.Print("test")
	if !strings.Contains(buf.String(), "test") {
		t.Errorf("Output() did not write to the buffer")
	}
}

func TestPrint(t *testing.T) {
	var buf bytes.Buffer
	origLogger := Logger
	Logger = Logger.Output(&buf)

	defer func() { Logger = origLogger }()

	Print("test")

	if !strings.Contains(buf.String(), "test") {
		t.Errorf("Print() did not write to the buffer")
	}
}

func TestPrintf(t *testing.T) {
	var buf bytes.Buffer
	origLogger := Logger
	Logger = Logger.Output(&buf)

	defer func() { Logger = origLogger }()

	Printf("test %s", "string")

	if !strings.Contains(buf.String(), "test string") {
		t.Errorf("Printf() did not write to the buffer")
	}
}

// TestLogLevels verifies that Info, Debug, Warn, Error, and Trace all write to
// the logger. Must not run in parallel because it temporarily swaps the global Logger.
func TestLogLevels(t *testing.T) {
	var buf bytes.Buffer
	origLogger := Logger
	Logger = Logger.Output(&buf).Level(zerolog.TraceLevel)

	defer func() { Logger = origLogger }()

	Trace().Msg("test-trace")
	Debug().Msg("test-debug")
	Info().Msg("test-info")
	Warn().Msg("test-warn")
	Error().Msg("test-error")

	output := buf.String()

	for _, expected := range []string{"test-trace", "test-debug", "test-info", "test-warn", "test-error"} {
		if !strings.Contains(output, expected) {
			t.Errorf("log level function did not write %q to the buffer", expected)
		}
	}
}

func TestLogLevelHelpers(t *testing.T) {
	t.Parallel()
	// Verify each helper returns a non-nil *zerolog.Event without panicking.
	var buf bytes.Buffer
	l := Output(&buf)

	if e := l.Trace(); e == nil {
		t.Error("Trace() returned nil")
	}

	if e := l.Debug(); e == nil {
		t.Error("Debug() returned nil")
	}

	if e := l.Info(); e == nil {
		t.Error("Info() returned nil")
	}

	if e := l.Warn(); e == nil {
		t.Error("Warn() returned nil")
	}

	if e := l.Error(); e == nil {
		t.Error("Error() returned nil")
	}
}

func TestWith(t *testing.T) {
	t.Parallel()
	// With() returns a zerolog.Context — verify it is non-zero.
	ctx := With()
	l := ctx.Logger()

	var buf bytes.Buffer

	l = l.Output(&buf)
	l.Info().Msg("with-test")

	if !strings.Contains(buf.String(), "with-test") {
		t.Error("With() context did not produce a usable logger")
	}
}

func TestLevel(t *testing.T) {
	t.Parallel()
	// Level() returns a filtered logger.
	var buf bytes.Buffer
	l := Level(zerolog.InfoLevel).Output(&buf)
	l.Debug().Msg("should-not-appear")
	l.Info().Msg("should-appear")

	output := buf.String()

	if strings.Contains(output, "should-not-appear") {
		t.Error("Level(Info) should filter out Debug messages")
	}

	if !strings.Contains(output, "should-appear") {
		t.Error("Level(Info) should pass through Info messages")
	}
}

func TestErrHelper(t *testing.T) {
	var buf bytes.Buffer
	origLogger := Logger
	Logger = Logger.Output(&buf).Level(zerolog.TraceLevel)

	defer func() { Logger = origLogger }()

	Err(nil).Msg("err-nil-test")

	if !strings.Contains(buf.String(), "err-nil-test") {
		t.Error("Err(nil) did not write to the buffer")
	}
}

func TestLogHelper(t *testing.T) {
	var buf bytes.Buffer
	origLogger := Logger
	Logger = Logger.Output(&buf)

	defer func() { Logger = origLogger }()

	Log().Msg("log-test")

	if !strings.Contains(buf.String(), "log-test") {
		t.Error("Log() did not write to the buffer")
	}
}

func TestWithLevel(t *testing.T) {
	var buf bytes.Buffer
	origLogger := Logger
	Logger = Logger.Output(&buf)

	defer func() { Logger = origLogger }()

	WithLevel(zerolog.InfoLevel).Msg("withlevel-test")

	if !strings.Contains(buf.String(), "withlevel-test") {
		t.Error("WithLevel() did not write to the buffer")
	}
}

func TestCtx(t *testing.T) {
	t.Parallel()
	// Ctx returns the logger associated with a context; a plain context returns a disabled logger.
	ctx := zerolog.New(nil).WithContext(t.Context())
	l := Ctx(ctx)

	if l == nil {
		t.Error("Ctx() returned nil")
	}
}
