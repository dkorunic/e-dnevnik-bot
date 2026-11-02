// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/rs/zerolog"
)

// Grades and teachers' notes are children's data: at the default level the log,
// which docker logs and journald keep, must name the event and nothing more.
// -0 opts in to the content at Debug.
// Not parallel: swaps the global logger and flag pointers.
func TestDefaultLogLevelOmitsEventContent(t *testing.T) {
	var buf bytes.Buffer

	orig := logger.Logger
	logger.Logger = zerolog.New(&buf).Level(zerolog.InfoLevel)

	t.Cleanup(func() { logger.Logger = orig })

	setRelevancePeriod(t, 24*time.Hour)
	setReadingList(t, false)

	eDB := existingDB(t, filepath.Join(t.TempDir(), "privacy.db.sqlite"))

	alert := grade("alerted")
	alert.Fields = []string{time.Now().Format(formatHRDateOnly), "SECRETNOTE"}

	oldGrade := grade("old grade")
	oldGrade.Fields = []string{time.Now().AddDate(0, -2, 0).Format(formatHRDateOnly), "SECRETNOTE"}

	badDate := grade("bad date")
	badDate.Fields = []string{"soon", "SECRETNOTE"}

	oldExam := msgtypes.Message{
		Code:      msgtypes.Exam,
		Username:  "testuser",
		Subject:   "old exam",
		Timestamp: time.Now().AddDate(0, -2, 0),
		Fields:    []string{"SECRETNOTE"},
	}

	runDedup(t, t.Context(), eDB, alert, oldGrade, badDate, oldExam)

	out := buf.String()
	if strings.Contains(out, "SECRETNOTE") {
		t.Errorf("log carries event content:\n%s", out)
	}

	if !strings.Contains(out, "New alert for: testuser/alerted") {
		t.Errorf("log lost the alert line:\n%s", out)
	}
}
