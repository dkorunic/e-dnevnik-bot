package messenger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
	"go.uber.org/ratelimit"
	"google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

func TestProcessCalendar(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	// Create a temporary database for testing.
	tmpdir, err := os.MkdirTemp("", "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	eDB, err := sqlitedb.New(tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	processCalendar(ctx, eDB, msg, time.Now(), rl, srv, "primary", 1)
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

	// Test with a named calendar
	calID := getCalendarID(srv, "Test Calendar")
	if calID != "test-id" {
		t.Errorf("getCalendarID() = %s, want test-id", calID)
	}

	// Test with the primary calendar
	calID = getCalendarID(srv, "")
	if calID != "primary" {
		t.Errorf("getCalendarID() = %s, want primary", calID)
	}
}
