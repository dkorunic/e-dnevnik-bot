// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"go.uber.org/ratelimit"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

// calendarTestDB opens a throwaway database for the Calendar path tests.
func calendarTestDB(t *testing.T) *sqlitedb.Edb {
	t.Helper()

	eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), "calendar.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	return eDB
}

// calendarStub serves the Events.Insert endpoint with the given status, and
// counts how many inserts it saw.
func calendarStub(t *testing.T, status int, body string) (*calendar.Service, *atomic.Int32) {
	t.Helper()

	var inserts atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/events") {
			inserts.Add(1)
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))

	t.Cleanup(srv.Close)

	svc, err := calendar.NewService(t.Context(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("calendar.NewService() failed: %v", err)
	}

	return svc, &inserts
}

// futureExam builds an exam dated tomorrow, with scrape's three-field layout.
func futureExam() msgtypes.Message {
	return msgtypes.Message{
		Code:      msgtypes.Exam,
		Timestamp: time.Now().Add(24 * time.Hour),
		Username:  "pero.peric",
		Subject:   "Matematika",
		Fields:    []string{"Matematika", "01.09.2026.", "Pisana provjera"},
	}
}

// TestProcessCalendarSkipsNonExams: Calendar only ever receives exams. Anything
// else reaching an insert would put grades into the user's calendar.
func TestProcessCalendarSkipsNonExams(t *testing.T) {
	t.Parallel()

	for _, code := range []msgtypes.EventCode{
		msgtypes.Grade, msgtypes.Reading, msgtypes.FinalGrade, msgtypes.NationalExam,
	} {
		eDB := calendarTestDB(t)
		svc, inserts := calendarStub(t, http.StatusOK, `{}`)

		g := futureExam()
		g.Code = code

		processCalendar(t.Context(), eDB, g, ratelimit.NewUnlimited(), svc, "primary", 1)

		if n := inserts.Load(); n != 0 {
			t.Errorf("code %v triggered %d calendar inserts, want 0", code, n)
		}
	}
}

// TestProcessCalendarSkipsFieldlessExam: a legacy or truncated queue entry with
// no fields would create a contentless calendar event.
func TestProcessCalendarSkipsFieldlessExam(t *testing.T) {
	t.Parallel()

	eDB := calendarTestDB(t)
	svc, inserts := calendarStub(t, http.StatusOK, `{}`)

	g := futureExam()
	g.Fields = nil

	processCalendar(t.Context(), eDB, g, ratelimit.NewUnlimited(), svc, "primary", 1)

	if n := inserts.Load(); n != 0 {
		t.Errorf("a field-less exam triggered %d inserts, want 0", n)
	}
}

// TestProcessCalendarExamDayBoundary is the subtle one the code comments call
// out: exam timestamps are midnight-UTC all-day markers, so comparing them
// against time.Now() as an instant would treat an exam first seen *on* the exam
// day as already past and silently drop it. Only strictly-earlier dates skip.
func TestProcessCalendarExamDayBoundary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		timestamp  time.Time
		wantInsert bool
	}{
		{
			name:       "tomorrow is inserted",
			timestamp:  time.Now().UTC().AddDate(0, 0, 1).Truncate(24 * time.Hour),
			wantInsert: true,
		},
		{
			name:       "today at midnight UTC is still inserted",
			timestamp:  time.Now().UTC().Truncate(24 * time.Hour),
			wantInsert: true,
		},
		{
			name:       "yesterday is skipped",
			timestamp:  time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour),
			wantInsert: false,
		},
		{
			name:       "last month is skipped",
			timestamp:  time.Now().UTC().AddDate(0, -1, 0).Truncate(24 * time.Hour),
			wantInsert: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eDB := calendarTestDB(t)
			svc, inserts := calendarStub(t, http.StatusOK, `{}`)

			g := futureExam()
			g.Timestamp = tt.timestamp

			processCalendar(t.Context(), eDB, g, ratelimit.NewUnlimited(), svc, "primary", 1)

			if got := inserts.Load() > 0; got != tt.wantInsert {
				t.Errorf("insert happened = %v, want %v — an exam on its own day must still reach the calendar", got, tt.wantInsert)
			}
		})
	}
}

// TestProcessCalendarConflictIsIdempotentSuccess: the event ID is a
// deterministic hash, so a retried insert comes back 409. That must count as
// success — requeueing it would retry the same conflict every cycle until
// MaxQueueAge.
func TestProcessCalendarConflictIsIdempotentSuccess(t *testing.T) {
	t.Parallel()

	eDB := calendarTestDB(t)
	svc, _ := calendarStub(t, http.StatusConflict,
		`{"error":{"code":409,"message":"The requested identifier already exists."}}`)

	processCalendar(t.Context(), eDB, futureExam(), ratelimit.NewUnlimited(), svc, "primary", 1)

	if got := queue.FetchFailedMsgs(t.Context(), eDB, CalendarQueueName); len(got) != 0 {
		t.Errorf("a 409 requeued %+v; a duplicate insert is an idempotent success, not a failure", got)
	}
}

// TestProcessCalendarPermanentErrorIsDropped: a 403/404 will fail identically
// forever, so it is poison-dropped rather than requeued. Requeueing would retry
// it every cycle until MaxQueueAge, with a loud error each time.
func TestProcessCalendarPermanentErrorIsDropped(t *testing.T) {
	t.Parallel()

	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound} {
		eDB := calendarTestDB(t)
		svc, _ := calendarStub(t, status, `{"error":{"code":`+strconv.Itoa(status)+`,"message":"nope"}}`)

		processCalendar(t.Context(), eDB, futureExam(), ratelimit.NewUnlimited(), svc, "primary", 1)

		if got := queue.FetchFailedMsgs(t.Context(), eDB, CalendarQueueName); len(got) != 0 {
			t.Errorf("status %d requeued %+v; a permanent failure must be dropped, not retried forever", status, got)
		}
	}
}

// TestProcessCalendarTransientErrorIsRequeued: a 5xx may well succeed next
// cycle. The event is already dedup-flagged, so dropping it here loses it.
func TestProcessCalendarTransientErrorIsRequeued(t *testing.T) {
	t.Parallel()

	eDB := calendarTestDB(t)
	svc, _ := calendarStub(t, http.StatusServiceUnavailable,
		`{"error":{"code":503,"message":"backend error"}}`)

	g := futureExam()

	processCalendar(t.Context(), eDB, g, ratelimit.NewUnlimited(), svc, "primary", 1)

	got := queue.FetchFailedMsgs(t.Context(), eDB, CalendarQueueName)
	if len(got) != 1 || got[0].Msg.Subject != g.Subject {
		t.Fatalf("FetchFailedMsgs = %+v, want the exam requeued after a transient failure", got)
	}
}

// TestProcessCalendarCancelledBeforeInsertRequeues covers the pre-insert
// shutdown guard: the caller's tail-slice has already moved past this message,
// so it must be persisted rather than dropped when the context is already done.
func TestProcessCalendarCancelledBeforeInsertRequeues(t *testing.T) {
	t.Parallel()

	eDB := calendarTestDB(t)
	svc, inserts := calendarStub(t, http.StatusOK, `{}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	processCalendar(ctx, eDB, futureExam(), ratelimit.NewUnlimited(), svc, "primary", 1)

	if n := inserts.Load(); n != 0 {
		t.Errorf("insert was attempted %d times on an already-cancelled context, want 0", n)
	}

	if got := queue.FetchFailedMsgs(context.Background(), eDB, CalendarQueueName); len(got) != 1 {
		t.Fatalf("FetchFailedMsgs = %+v, want the exam persisted despite shutdown", got)
	}
}

// TestGetCalendarIDPaginates: a user with many calendars gets a paged
// CalendarList. Stopping after the first page would silently fall back to no
// calendar for anyone whose target sits on a later page.
func TestGetCalendarIDPaginates(t *testing.T) {
	t.Parallel()

	var pages atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Query().Get("pageToken") == "" {
			pages.Add(1)
			_, _ = w.Write([]byte(`{"items":[{"id":"other-id","summary":"Neki drugi"}],"nextPageToken":"page2"}`))

			return
		}

		pages.Add(1)
		_, _ = w.Write([]byte(`{"items":[{"id":"wanted-id","summary":"e-Dnevnik"}]}`))
	}))
	defer srv.Close()

	svc, err := calendar.NewService(t.Context(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("calendar.NewService() failed: %v", err)
	}

	if got := getCalendarID(t.Context(), svc, "e-Dnevnik"); got != "wanted-id" {
		t.Errorf("getCalendarID() = %q, want %q — the calendar list must be followed past the first page", got, "wanted-id")
	}

	if n := pages.Load(); n < 2 {
		t.Errorf("fetched %d pages, want at least 2", n)
	}
}

// TestGetCalendarIDNoMatch: an unknown calendar name must resolve to the empty
// string so the caller can report a configuration error, not silently fall back
// to the primary calendar and scatter exams into it.
func TestGetCalendarIDNoMatch(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"id":"other-id","summary":"Neki drugi"}]}`))
	}))
	defer srv.Close()

	svc, err := calendar.NewService(t.Context(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("calendar.NewService() failed: %v", err)
	}

	if got := getCalendarID(t.Context(), svc, "Ne postoji"); got != "" {
		t.Errorf("getCalendarID() = %q, want empty for an unknown calendar name", got)
	}
}

// TestGetCalendarIDListError: an API failure must not be mistaken for "no such
// calendar name" in a way that writes into the wrong calendar.
func TestGetCalendarIDListError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":500,"message":"backend error"}}`))
	}))
	defer srv.Close()

	svc, err := calendar.NewService(t.Context(), option.WithEndpoint(srv.URL), option.WithoutAuthentication())
	if err != nil {
		t.Fatalf("calendar.NewService() failed: %v", err)
	}

	if got := getCalendarID(t.Context(), svc, "e-Dnevnik"); got != "" {
		t.Errorf("getCalendarID() = %q, want empty when the calendar list cannot be read", got)
	}
}
