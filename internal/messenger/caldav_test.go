// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"go.uber.org/ratelimit"
)

const caldavCollectionPath = "/dav/calendars/pero/ispiti/"

// caldavRequest is one request as the stub saw it.
type caldavRequest struct {
	Header http.Header
	Method string
	Path   string
	Body   string
}

// caldavStub answers every request with status (3xx carries a Location) and
// records what it saw. It returns the collection URL to configure.
func caldavStub(t *testing.T, status int) (string, func() []caldavRequest) {
	t.Helper()

	var (
		mu   sync.Mutex
		seen []caldavRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		seen = append(seen, caldavRequest{Header: r.Header.Clone(), Method: r.Method, Path: r.URL.Path, Body: string(body)})
		mu.Unlock()

		if status >= 300 && status < 400 {
			w.Header().Set("Location", "/moved/")
		}

		w.WriteHeader(status)
	}))

	t.Cleanup(srv.Close)

	return srv.URL + caldavCollectionPath, func() []caldavRequest {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(seen)
	}
}

func caldavTestConfig(collection string, retries uint) CalDAVConfig {
	return CalDAVConfig{URL: collection, Username: "pero", Password: "tajna-lozinka", Retries: retries}
}

// caldavSequenceStub answers successive requests with the statuses in seq (the
// last entry repeats once seq is exhausted) and records what it saw.
func caldavSequenceStub(t *testing.T, seq []int) (string, func() []caldavRequest) {
	t.Helper()

	var (
		mu   sync.Mutex
		seen []caldavRequest
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		mu.Lock()
		idx := len(seen)
		seen = append(seen, caldavRequest{Header: r.Header.Clone(), Method: r.Method, Path: r.URL.Path, Body: string(body)})
		mu.Unlock()

		status := seq[min(idx, len(seq)-1)]

		w.WriteHeader(status)
	}))

	t.Cleanup(srv.Close)

	return srv.URL + caldavCollectionPath, func() []caldavRequest {
		mu.Lock()
		defer mu.Unlock()

		return slices.Clone(seen)
	}
}

// TestProcessCalDAVRequestShape pins the create-only PUT: resource name from
// the shared event ID, basic auth, iCalendar media type and If-None-Match: *,
// without which a retry would overwrite instead of answering 412.
func TestProcessCalDAVRequestShape(t *testing.T) {
	t.Parallel()

	collection, seen := caldavStub(t, http.StatusCreated)
	eDB := calendarTestDB(t)
	g := futureExam()

	processCalDAV(t.Context(), eDB, g, ratelimit.NewUnlimited(), caldavTestConfig(collection, 1))

	reqs := seen()
	if len(reqs) != 1 {
		t.Fatalf("server saw %d requests, want 1", len(reqs))
	}

	r := reqs[0]
	ev, _ := examEventOf("test", g)

	if r.Method != http.MethodPut {
		t.Errorf("method = %s, want PUT", r.Method)
	}

	if want := caldavCollectionPath + ev.ID + ".ics"; r.Path != want {
		t.Errorf("path = %q, want %q", r.Path, want)
	}

	user, pass, ok := (&http.Request{Header: r.Header}).BasicAuth()
	if !ok || user != "pero" || pass != "tajna-lozinka" {
		t.Errorf("basic auth = (%q, %q, %v), want (pero, tajna-lozinka, true)", user, pass, ok)
	}

	if got := r.Header.Get("Content-Type"); got != "text/calendar; charset=utf-8" {
		t.Errorf("Content-Type = %q", got)
	}

	if got := r.Header.Get("If-None-Match"); got != "*" {
		t.Errorf("If-None-Match = %q, want * — without it a repeat overwrites instead of answering 412", got)
	}

	// The event ID is a 64-hex SHA-256 digest, so "UID:"+ID+CalDAVUIDSuffix is
	// 82 octets — over ical.go's 75-octet fold limit — and always wraps onto a
	// continuation line. Undo the RFC 5545 fold (a lone space after CRLF) before
	// checking, exactly as a reader would.
	unfolded := strings.ReplaceAll(r.Body, "\r\n ", "")
	if !strings.Contains(unfolded, "UID:"+ev.ID+CalDAVUIDSuffix+"\r\n") {
		t.Errorf("body lacks the expected UID:\n%s", r.Body)
	}

	if got := queue.FetchFailedMsgs(t.Context(), eDB, CalDAVQueueName); len(got) != 0 {
		t.Errorf("a 201 queued %+v", got)
	}
}

// TestProcessCalDAVCollectionWithoutTrailingSlash: users paste collection URLs
// both ways; the resource must land inside the collection either way.
func TestProcessCalDAVCollectionWithoutTrailingSlash(t *testing.T) {
	t.Parallel()

	collection, seen := caldavStub(t, http.StatusCreated)
	g := futureExam()

	processCalDAV(t.Context(), calendarTestDB(t), g, ratelimit.NewUnlimited(),
		caldavTestConfig(strings.TrimSuffix(collection, "/"), 1))

	ev, _ := examEventOf("test", g)

	reqs := seen()
	if len(reqs) != 1 || reqs[0].Path != caldavCollectionPath+ev.ID+".ics" {
		t.Fatalf("requests = %+v, want one PUT to %s%s.ics", reqs, caldavCollectionPath, ev.ID)
	}
}

// TestProcessCalDAVStatusHandling: success and 412 finish; permanent 4xx are
// poison-dropped after exactly one attempt; transient statuses retry and then
// requeue, because the exam is already dedup-flagged and would otherwise be lost.
func TestProcessCalDAVStatusHandling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		status     int
		retries    uint
		wantPuts   int
		wantQueued bool
	}{
		{name: "201 created", status: http.StatusCreated, retries: 3, wantPuts: 1},
		{name: "204 no content", status: http.StatusNoContent, retries: 3, wantPuts: 1},
		{name: "412 already stored is success", status: http.StatusPreconditionFailed, retries: 3, wantPuts: 1},
		{name: "400 dropped", status: http.StatusBadRequest, retries: 3, wantPuts: 1},
		{name: "401 dropped", status: http.StatusUnauthorized, retries: 3, wantPuts: 1},
		{name: "403 dropped", status: http.StatusForbidden, retries: 3, wantPuts: 1},
		{name: "404 dropped", status: http.StatusNotFound, retries: 3, wantPuts: 1},
		{name: "405 not a collection, dropped", status: http.StatusMethodNotAllowed, retries: 3, wantPuts: 1},
		{name: "415 dropped", status: http.StatusUnsupportedMediaType, retries: 3, wantPuts: 1},
		{name: "408 retried then queued", status: http.StatusRequestTimeout, retries: 2, wantPuts: 2, wantQueued: true},
		{name: "429 retried then queued", status: http.StatusTooManyRequests, retries: 2, wantPuts: 2, wantQueued: true},
		{name: "500 retried then queued", status: http.StatusInternalServerError, retries: 2, wantPuts: 2, wantQueued: true},
		{name: "503 retried then queued", status: http.StatusServiceUnavailable, retries: 2, wantPuts: 2, wantQueued: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			collection, seen := caldavStub(t, tt.status)
			eDB := calendarTestDB(t)

			processCalDAV(t.Context(), eDB, futureExam(), ratelimit.NewUnlimited(), caldavTestConfig(collection, tt.retries))

			if got := len(seen()); got != tt.wantPuts {
				t.Errorf("server saw %d PUTs, want %d", got, tt.wantPuts)
			}

			queued := queue.FetchFailedMsgs(t.Context(), eDB, CalDAVQueueName)
			if got := len(queued) == 1; got != tt.wantQueued || len(queued) > 1 {
				t.Errorf("queued %d rows, want queued=%v", len(queued), tt.wantQueued)
			}
		})
	}
}

// TestPutCalDAVPreconditionFailedIsSuccess: If-None-Match: * means the event
// is already stored, so putCalDAV must treat a 412 as success at the source.
// A post-loop check keyed on retry-go's error tree would otherwise miss this
// after a preceding transient failure (see
// TestProcessCalDAV502ThenPreconditionFailedIsNotQueued) because
// errors.AsType finds the FIRST attempt's error, not the 412.
func TestPutCalDAVPreconditionFailedIsSuccess(t *testing.T) {
	t.Parallel()

	collection, _ := caldavStub(t, http.StatusPreconditionFailed)

	target, err := url.JoinPath(collection, "abc123.ics")
	if err != nil {
		t.Fatalf("url.JoinPath() failed: %v", err)
	}

	cfg := caldavTestConfig(collection, 1)

	if err := putCalDAV(t.Context(), cfg, target, []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n")); err != nil {
		t.Errorf("putCalDAV() = %v, want nil for a 412 (already stored)", err)
	}
}

// TestProcessCalDAV502ThenPreconditionFailedIsNotQueued: a transient 502
// followed by a 412 must finish as a stored event, not a permanent-failure
// drop or a requeue — exactly two PUTs, and nothing left in the queue.
func TestProcessCalDAV502ThenPreconditionFailedIsNotQueued(t *testing.T) {
	t.Parallel()

	collection, seen := caldavSequenceStub(t, []int{http.StatusBadGateway, http.StatusPreconditionFailed})
	eDB := calendarTestDB(t)

	processCalDAV(t.Context(), eDB, futureExam(), ratelimit.NewUnlimited(), caldavTestConfig(collection, 2))

	if got := len(seen()); got != 2 {
		t.Fatalf("server saw %d PUTs, want 2", got)
	}

	if got := queue.FetchFailedMsgs(t.Context(), eDB, CalDAVQueueName); len(got) != 0 {
		t.Errorf("queued %+v; a 502 then 412 means the event is stored, not queued", got)
	}
}

// TestPutCalDAVRedactsRedirectLocationUserinfo: a Location header can carry
// embedded credentials; the resulting error string must never leak them.
func TestPutCalDAVRedactsRedirectLocationUserinfo(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://u:secret@host/x/")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(srv.Close)

	collection := srv.URL + caldavCollectionPath

	target, err := url.JoinPath(collection, "abc123.ics")
	if err != nil {
		t.Fatalf("url.JoinPath() failed: %v", err)
	}

	err = putCalDAV(t.Context(), caldavTestConfig(collection, 1), target, []byte("BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n"))
	if err == nil {
		t.Fatal("putCalDAV() = nil, want an error for a 302")
	}

	if strings.Contains(err.Error(), "secret") {
		t.Errorf("error leaks redirect userinfo: %v", err)
	}
}

// TestProcessCalDAVDoesNotFollowRedirects: net/http replays a redirected PUT as
// a bodiless GET, whose 200 would read as a stored exam. The client must stop
// at the 3xx and treat it as a configuration error.
func TestProcessCalDAVDoesNotFollowRedirects(t *testing.T) {
	t.Parallel()

	for _, status := range []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		collection, seen := caldavStub(t, status)
		eDB := calendarTestDB(t)

		processCalDAV(t.Context(), eDB, futureExam(), ratelimit.NewUnlimited(), caldavTestConfig(collection, 3))

		reqs := seen()
		if len(reqs) != 1 || reqs[0].Method != http.MethodPut {
			t.Errorf("status %d: server saw %+v, want exactly one PUT and no followed redirect", status, reqs)
		}

		if got := queue.FetchFailedMsgs(t.Context(), eDB, CalDAVQueueName); len(got) != 0 {
			t.Errorf("status %d queued %+v; a redirect is a config error, not transient", status, got)
		}
	}
}

// TestProcessCalDAVSkipsWhatCalendarsMustNotReceive: nothing reaches the server
// for non-exams, past exams or field-less exams.
func TestProcessCalDAVSkipsWhatCalendarsMustNotReceive(t *testing.T) {
	t.Parallel()

	mutations := map[string]func(*msgtypes.Message){
		"grade":     func(g *msgtypes.Message) { g.Code = msgtypes.Grade },
		"yesterday": func(g *msgtypes.Message) { g.Timestamp = time.Now().UTC().AddDate(0, 0, -1).Truncate(24 * time.Hour) },
		"no fields": func(g *msgtypes.Message) { g.Fields = nil },
	}

	for name, mutate := range mutations {
		collection, seen := caldavStub(t, http.StatusCreated)

		g := futureExam()
		mutate(&g)

		processCalDAV(t.Context(), calendarTestDB(t), g, ratelimit.NewUnlimited(), caldavTestConfig(collection, 1))

		if n := len(seen()); n != 0 {
			t.Errorf("%s: server saw %d requests, want 0", name, n)
		}
	}
}

// TestProcessCalDAVCancelledBeforePutRequeues: the caller has already consumed
// the message, so a shutdown must persist it rather than drop it.
func TestProcessCalDAVCancelledBeforePutRequeues(t *testing.T) {
	t.Parallel()

	collection, seen := caldavStub(t, http.StatusCreated)
	eDB := calendarTestDB(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	processCalDAV(ctx, eDB, futureExam(), ratelimit.NewUnlimited(), caldavTestConfig(collection, 1))

	if n := len(seen()); n != 0 {
		t.Errorf("server saw %d requests on a cancelled context, want 0", n)
	}

	if got := queue.FetchFailedMsgs(context.Background(), eDB, CalDAVQueueName); len(got) != 1 {
		t.Fatalf("FetchFailedMsgs = %+v, want the exam persisted despite shutdown", got)
	}
}

// TestProcessCalDAVTransportErrorRequeues: an unreachable server is transient.
func TestProcessCalDAVTransportErrorRequeues(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.NotFoundHandler())
	collection := srv.URL + caldavCollectionPath
	srv.Close()

	eDB := calendarTestDB(t)

	processCalDAV(t.Context(), eDB, futureExam(), ratelimit.NewUnlimited(), caldavTestConfig(collection, 1))

	if got := queue.FetchFailedMsgs(t.Context(), eDB, CalDAVQueueName); len(got) != 1 {
		t.Fatalf("FetchFailedMsgs = %+v, want the exam requeued after a connection failure", got)
	}
}

// TestCalDAVResendsQueuedThenDrainsChannel covers the entry point's lifecycle:
// last cycle's failures go first, then the live channel, and the queue ends
// empty.
func TestCalDAVResendsQueuedThenDrainsChannel(t *testing.T) {
	t.Parallel()

	collection, seen := caldavStub(t, http.StatusCreated)
	eDB := calendarTestDB(t)

	old := futureExam()
	old.Subject = "Fizika"

	if err := queue.StoreFailedMsgs(t.Context(), eDB, CalDAVQueueName, old); err != nil {
		t.Fatalf("StoreFailedMsgs() failed: %v", err)
	}

	ch := make(chan msgtypes.Message, 1)
	ch <- futureExam()
	close(ch)

	if err := CalDAV(t.Context(), eDB, ch, caldavTestConfig(collection, 1)); err != nil {
		t.Fatalf("CalDAV() = %v", err)
	}

	reqs := seen()
	if len(reqs) != 2 {
		t.Fatalf("server saw %d requests, want 2 (one resend, one live)", len(reqs))
	}

	if !strings.Contains(reqs[0].Body, "Ispit iz: Fizika") {
		t.Errorf("first PUT was not the queued exam:\n%s", reqs[0].Body)
	}

	if !strings.Contains(reqs[1].Body, "Ispit iz: Matematika") {
		t.Errorf("second PUT was not the live exam:\n%s", reqs[1].Body)
	}

	if got := queue.FetchFailedMsgs(t.Context(), eDB, CalDAVQueueName); len(got) != 0 {
		t.Errorf("queue not empty after successful resend: %+v", got)
	}
}

// TestCalDAVLive runs against a real server when EDNEVNIK_CALDAV_TEST_URL
// (collection URL), _USER and _PASS are set, e.g. a local Radicale. It inserts
// a far-future exam twice: the second must hit 412 and still count as success.
func TestCalDAVLive(t *testing.T) {
	collection := os.Getenv("EDNEVNIK_CALDAV_TEST_URL")
	if collection == "" {
		t.Skip("EDNEVNIK_CALDAV_TEST_URL not set")
	}

	cfg := CalDAVConfig{
		URL:      collection,
		Username: os.Getenv("EDNEVNIK_CALDAV_TEST_USER"),
		Password: os.Getenv("EDNEVNIK_CALDAV_TEST_PASS"),
		Retries:  1,
	}

	eDB := calendarTestDB(t)
	g := futureExam()
	g.Timestamp = time.Date(2099, 9, 24, 0, 0, 0, 0, time.UTC)

	processCalDAV(t.Context(), eDB, g, ratelimit.NewUnlimited(), cfg)
	processCalDAV(t.Context(), eDB, g, ratelimit.NewUnlimited(), cfg)

	if got := queue.FetchFailedMsgs(t.Context(), eDB, CalDAVQueueName); len(got) != 0 {
		t.Fatalf("live insert queued %+v; check server logs", got)
	}
}
