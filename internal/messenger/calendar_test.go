// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/rs/zerolog"
	"go.uber.org/ratelimit"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

func TestProcessCalendar(t *testing.T) {
	t.Parallel()
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{}`))
	}))
	defer server.Close()

	ctx := context.Background()
	srv, err := calendar.NewService(ctx, option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("Unable to create calendar service: %v", err)
	}

	msg := msgtypes.Message{
		Code:      msgtypes.Exam,
		Timestamp: time.Now().Add(24 * time.Hour),
		Username:  "testuser",
		Subject:   "Test Subject",
		Fields:    []string{"field1"},
	}

	rl := ratelimit.New(1)

	tmpdir, err := os.MkdirTemp("", "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	eDB, err := sqlitedb.New(context.Background(), tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	processCalendar(ctx, eDB, msg, rl, srv, "primary", 1)

	want := `"reminders":{"overrides":[{"method":"popup","minutes":3780}],"useDefault":false}`
	if !strings.Contains(string(body), want) {
		t.Errorf("insert body %s lacks %s", body, want)
	}
}

func TestGetCalendarID(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "users/me/calendarList") {
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(`{"items": [{"id": "test-id", "summary": "Test Calendar"}]}`))
		}
	}))
	defer server.Close()

	ctx := context.Background()
	srv, err := calendar.NewService(ctx, option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("Unable to create calendar service: %v", err)
	}

	calID := getCalendarID(ctx, srv, "Test Calendar")
	if calID != "test-id" {
		t.Errorf("getCalendarID() = %s, want test-id", calID)
	}

	// Empty name must resolve to "primary".
	calID = getCalendarID(ctx, srv, "")
	if calID != "primary" {
		t.Errorf("getCalendarID() = %s, want primary", calID)
	}

	// The literal "primary" alias must also resolve to the primary calendar —
	// its Summary is the owner's e-mail address and can never match by name.
	calID = getCalendarID(ctx, srv, "primary")
	if calID != "primary" {
		t.Errorf("getCalendarID() = %s, want primary for the literal alias", calID)
	}

	// Surrounding spaces must not turn the alias into a failed name lookup.
	calID = getCalendarID(ctx, srv, " Primary ")
	if calID != "primary" {
		t.Errorf("getCalendarID() = %s, want primary for a padded alias", calID)
	}

	// Name matching is case-insensitive and space-tolerant.
	calID = getCalendarID(ctx, srv, "  test calendar ")
	if calID != "test-id" {
		t.Errorf("getCalendarID() = %s, want test-id for case/space-tolerant match", calID)
	}
}

// TestProcessCalendarConflictAfterTransientIsSuccess: an attempt can fail
// after Google stored the event, so the retry answers 409. That is success.
// Deciding it after retry.Do read the first attempt's 503 instead, and logged
// a false "Permanently dropping" for an event that exists.
// Not parallel: swaps the package-level logger.
func TestProcessCalendarConflictAfterTransientIsSuccess(t *testing.T) {
	var calls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"code":503,"message":"backend error"}}`))

			return
		}

		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":409,"message":"The requested identifier already exists."}}`))
	}))
	defer server.Close()

	srv, err := calendar.NewService(t.Context(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("Unable to create calendar service: %v", err)
	}

	var buf bytes.Buffer

	orig := logger.Logger
	logger.Logger = zerolog.New(&buf)

	t.Cleanup(func() { logger.Logger = orig })

	eDB := calendarTestDB(t)

	processCalendar(t.Context(), eDB, futureExam(), ratelimit.NewUnlimited(), srv, "primary", 3)

	if got := calls.Load(); got != 2 {
		t.Errorf("server saw %d inserts, want 2 (503, then 409)", got)
	}

	if strings.Contains(buf.String(), "Permanently dropping") {
		t.Errorf("a 409 after a transient failure was logged as a drop: %s", buf.String())
	}

	if q := queue.FetchFailedMsgs(t.Context(), eDB, CalendarQueueName); len(q) != 0 {
		t.Errorf("queued %d rows for an event that already exists", len(q))
	}
}
