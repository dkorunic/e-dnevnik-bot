package logger

import (
	"bytes"
	"strings"
	"testing"
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
	t.Parallel()
	var buf bytes.Buffer
	Logger = Logger.Output(&buf)
	Print("test")
	if !strings.Contains(buf.String(), "test") {
		t.Errorf("Print() did not write to the buffer")
	}
}

func TestPrintf(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	Logger = Logger.Output(&buf)
	Printf("test %s", "string")
	if !strings.Contains(buf.String(), "test string") {
		t.Errorf("Printf() did not write to the buffer")
	}
}
