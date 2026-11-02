// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/messenger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// A failed queue write leaves the outbox row the only copy of a flagged event:
// clearing it anyway lost the alert for good. The next cycle recovers it instead.
// Not parallel: mutates package-level flag pointers and the scrapeStage seam.
func TestRunPollCycleKeepsOutboxAfterFailedQueueWrite(t *testing.T) {
	setRelevancePeriod(t, 0)
	setReadingList(t, false)
	resetExitLatch(t)

	path := setDBFile(t, "failed-write.db")

	eDB, err := sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	if err := eDB.Put(t.Context(), seedMigratedKey, []byte{1}); err != nil {
		t.Fatalf("Put() = %v", err)
	}

	if !putSeedMarker(t.Context(), eDB, "u") {
		t.Fatal("putSeedMarker() failed")
	}

	_ = eDB.Close()

	admin, err := sql.Open("sqlite", "file:"+path+".sqlite")
	if err != nil {
		t.Fatalf("sql.Open() = %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if _, err := admin.Exec(`CREATE TRIGGER no_queue BEFORE INSERT ON kv
		WHEN CAST(new.key AS TEXT) LIKE 'calendar-queue%' BEGIN SELECT RAISE(ABORT, 'full'); END`); err != nil {
		t.Fatalf("CREATE TRIGGER = %v", err)
	}

	exam := msgtypes.Message{
		Code:      msgtypes.Exam,
		Username:  "u",
		Subject:   "exam",
		Timestamp: time.Now().Add(72 * time.Hour),
		Fields:    []string{"x"},
	}

	origStage := scrapeStage
	scrapeStage = func(_ context.Context, wg *sync.WaitGroup, ch chan<- msgtypes.Message, _ config.TomlConfig, _ *userSet) {
		wg.Go(func() { ch <- exam })
	}

	t.Cleanup(func() { scrapeStage = origStage })

	cfg := config.TomlConfig{User: []config.User{{Username: "u"}}, CalendarDeferred: true}

	runPollCycle(t.Context(), cfg)

	if _, err := admin.Exec(`DROP TRIGGER no_queue`); err != nil {
		t.Fatalf("DROP TRIGGER = %v", err)
	}

	// The next cycle scrapes the same event, now a duplicate; only recovery can
	// deliver it.
	runPollCycle(t.Context(), cfg)

	eDB, err = sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	if got := subjectsIn(t, eDB, messenger.CalendarQueueName); len(got) != 1 || got[0] != "exam" {
		t.Errorf("queue after recovery = %v, want the exam once", got)
	}

	if got := subjectsIn(t, eDB, queue.OutboxKey); len(got) != 0 {
		t.Errorf("outbox after recovery = %v, want it empty", got)
	}
}

func TestQuietWindowTooShortFor(t *testing.T) {
	tests := []struct {
		window   string
		interval time.Duration
		want     bool
	}{
		{"00:00-23:59", time.Hour, true},
		{"06:00-05:00", 2 * time.Hour, true},
		{"23:00-06:00", time.Hour, false},
		{"23:00-06:00", 24 * time.Hour, false},
		{"00:00-23:59", 24 * time.Hour, false}, // daily anyway
		{"off", 48 * time.Hour, false},
		{"23:00-06:00", 48 * time.Hour, false},
	}

	for _, tt := range tests {
		var q quietWindow
		if err := q.Set(tt.window); err != nil {
			t.Fatalf("Set(%q) = %v", tt.window, err)
		}

		if got := q.tooShortFor(tt.interval); got != tt.want {
			t.Errorf("%q tooShortFor(%v) = %v, want %v", tt.window, tt.interval, got, tt.want)
		}
	}
}
