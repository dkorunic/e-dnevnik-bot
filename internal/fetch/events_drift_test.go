// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/jordic/goics"
	"github.com/rs/zerolog"
)

// A date format the parser no longer knows drops every exam, leaving only "No
// scheduled exams" in the log: this warning is the sole sign of the drift.
// Not parallel: swaps the global logger.
func TestConsumeICalWarnsOnUnparseableDate(t *testing.T) {
	var buf bytes.Buffer

	orig := logger.Logger
	logger.Logger = zerolog.New(&buf)

	t.Cleanup(func() { logger.Logger = orig })

	ev := &goics.Event{Data: map[string]*goics.IcsNode{
		EventDateStart: {Val: "next tuesday"},
		EventSummary:   {Val: "Matematika"},
	}}

	var e Events
	if err := e.ConsumeICal(&goics.Calendar{Events: []*goics.Event{ev}}, nil); err != nil {
		t.Fatalf("ConsumeICal() = %v", err)
	}

	if len(e) != 0 {
		t.Fatalf("events = %+v, want the bad one skipped", e)
	}

	if out := buf.String(); !strings.Contains(out, `"level":"warn"`) || !strings.Contains(out, "next tuesday") {
		t.Errorf("log = %q, want a warning naming the date", out)
	}
}
