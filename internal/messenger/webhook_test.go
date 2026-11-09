// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"go.uber.org/ratelimit"
)

func webhookDB(t *testing.T) *sqlitedb.Edb {
	t.Helper()

	eDB, err := sqlitedb.New(context.Background(), filepath.Join(t.TempDir(), "webhook.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	return eDB
}

func webhookMsg() msgtypes.Message {
	return msgtypes.Message{
		Code:         msgtypes.Grade,
		Username:     "pero",
		Subject:      "Matematika",
		Descriptions: []string{"Datum", "Ocjena"},
		Fields:       []string{"8.9.", "5"},
	}
}

// statusServer answers every request with code, counting hits.
func statusServer(t *testing.T, code int, hits *atomic.Int32) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", "/elsewhere")
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)

	return srv
}

// deadURL fails at dial on every platform (EADDRNOTAVAIL or ECONNREFUSED). A
// freed ephemeral port would not: a parallel test's server can take it.
func deadURL(*testing.T) string {
	return "http://127.0.0.1:0/secret-token"
}

// TestWebhookPostsBothFormats pins the wire contract: JSON carries the
// Gotify-readable title/message plus the raw event, text is the plain alert,
// and configured headers ride along.
func TestWebhookPostsBothFormats(t *testing.T) {
	t.Parallel()

	type got struct {
		body, contentType, auth string
	}

	recv := make(chan got, 2)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		recv <- got{string(b), r.Header.Get("Content-Type"), r.Header.Get("Authorization")}
	}))
	t.Cleanup(srv.Close)

	cfg := WebhookConfig{Retries: 1, Hooks: []config.Webhook{
		{URL: srv.URL + "/json", Headers: map[string]string{"Authorization": "Bearer t0k"}},
		{URL: srv.URL + "/text", Format: config.WebhookText},
	}}

	eDB := webhookDB(t)

	if processWebhook(context.Background(), eDB, webhookMsg(), cfg, ratelimit.New(1000), map[string]int{}) {
		t.Fatal("processWebhook reported a cycle-stopping failure")
	}

	j, txt := <-recv, <-recv

	var p webhookPayload
	if err := json.Unmarshal([]byte(j.body), &p); err != nil {
		t.Fatalf("JSON body %q: %v", j.body, err)
	}

	if p.Event != "grade" || p.Username != "pero" || p.Subject != "Matematika" ||
		!strings.Contains(p.Title, "Matematika") || p.Message != "Datum: 8.9.\nOcjena: 5\n" {
		t.Errorf("payload = %+v", p)
	}

	if j.contentType != "application/json" || j.auth != "Bearer t0k" {
		t.Errorf("JSON request: Content-Type %q, Authorization %q", j.contentType, j.auth)
	}

	if !strings.HasPrefix(txt.contentType, "text/plain") || !strings.Contains(txt.body, "Ocjena: 5") || txt.auth != "" {
		t.Errorf("text request: Content-Type %q, Authorization %q, body %q", txt.contentType, txt.auth, txt.body)
	}

	if failed := queue.FetchFailedMsgs(context.Background(), eDB, WebhookQueueName); len(failed) != 0 {
		t.Errorf("queued %+v after a full delivery", failed)
	}
}

// TestWebhookUnknownStatusRequeuesOnce: a 401 or a 302 is not retried within the
// cycle (it would answer the same) but is requeued, never dropped. The redirect
// is not followed: that would turn the POST into a GET that "succeeds".
func TestWebhookUnknownStatusRequeuesOnce(t *testing.T) {
	t.Parallel()

	for _, code := range []int{http.StatusUnauthorized, http.StatusFound} {
		var hits atomic.Int32

		srv := statusServer(t, code, &hits)
		cfg := WebhookConfig{Retries: 3, Hooks: []config.Webhook{{URL: srv.URL}}}
		eDB := webhookDB(t)

		if processWebhook(context.Background(), eDB, webhookMsg(), cfg, ratelimit.New(1000), map[string]int{}) {
			t.Errorf("HTTP %d stopped the cycle; it concerns one endpoint", code)
		}

		if n := hits.Load(); n != 1 {
			t.Errorf("HTTP %d: %d requests, want 1 (no retry, no redirect follow)", code, n)
		}

		if failed := queue.FetchFailedMsgs(context.Background(), eDB, WebhookQueueName); len(failed) != 1 {
			t.Errorf("HTTP %d: queued %d messages, want 1", code, len(failed))
		}
	}
}

// TestWebhookDownEndpointSparesOthers: one unreachable endpoint must neither
// stop the cycle for the rest (each URL is its own server) nor be dialled again
// for every later message of the cycle.
func TestWebhookDownEndpointSparesOthers(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	ok := statusServer(t, http.StatusOK, &hits)
	dead := deadURL(t)

	cfg := WebhookConfig{Retries: 1, Hooks: []config.Webhook{{URL: dead}, {URL: ok.URL}}}
	eDB := webhookDB(t)
	down := map[string]int{}

	for range 2 {
		if processWebhook(context.Background(), eDB, webhookMsg(), cfg, ratelimit.New(1000), down) {
			t.Fatal("an unreachable endpoint stopped the cycle for the others")
		}
	}

	if n := hits.Load(); n != 2 {
		t.Errorf("healthy endpoint got %d requests, want 2", n)
	}

	if _, isDown := down[dead]; !isDown {
		t.Error("unreachable endpoint not remembered for the rest of the cycle")
	}

	failed := queue.FetchFailedMsgs(context.Background(), eDB, WebhookQueueName)
	if len(failed) != 2 {
		t.Fatalf("queued %d messages, want 2", len(failed))
	}

	for _, f := range failed {
		if !slices.Contains(f.Msg.SkipRecipients, ok.URL) || slices.Contains(f.Msg.SkipRecipients, dead) {
			t.Errorf("SkipRecipients = %v, want the healthy endpoint only", f.Msg.SkipRecipients)
		}
	}
}

// TestWebhookRepeatedServerErrorsSkipEndpoint: an endpoint answering 5xx spends
// the whole retry budget on each message while the poll cycle waits, so it is
// skipped after the second one. Every message stays queued.
func TestWebhookRepeatedServerErrorsSkipEndpoint(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	srv := statusServer(t, http.StatusBadGateway, &hits)
	cfg := WebhookConfig{Retries: 1, Hooks: []config.Webhook{{URL: srv.URL}}}
	eDB := webhookDB(t)
	down := map[string]int{}

	for range 4 {
		if processWebhook(context.Background(), eDB, webhookMsg(), cfg, ratelimit.New(1000), down) {
			t.Fatal("a server error stopped the cycle; it concerns one endpoint")
		}
	}

	if n := hits.Load(); n != 2 {
		t.Errorf("%d requests, want 2 before the endpoint is skipped", n)
	}

	if failed := queue.FetchFailedMsgs(context.Background(), eDB, WebhookQueueName); len(failed) != 4 {
		t.Errorf("queued %d messages, want 4", len(failed))
	}
}

// TestWebhookIsolatedServerErrorsKeepEndpoint: a 500 caused by one message must
// not skip the endpoint, or that message, retried first every cycle, would hold
// back everything queued behind it until MaxQueueAge.
func TestWebhookIsolatedServerErrorsKeepEndpoint(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1)%2 == 1 {
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)

	cfg := WebhookConfig{Retries: 1, Hooks: []config.Webhook{{URL: srv.URL}}}
	eDB := webhookDB(t)
	down := map[string]int{}

	for range 4 {
		processWebhook(context.Background(), eDB, webhookMsg(), cfg, ratelimit.New(1000), down)
	}

	if n := hits.Load(); n != 4 {
		t.Errorf("%d requests, want 4: a delivery in between clears the count", n)
	}

	if failed := queue.FetchFailedMsgs(context.Background(), eDB, WebhookQueueName); len(failed) != 2 {
		t.Errorf("queued %d messages, want 2", len(failed))
	}
}

// TestPostWebhookErrorHidesURL: url.Error quotes the full URL, and webhook paths
// hold secrets, so the logged error must not.
func TestPostWebhookErrorHidesURL(t *testing.T) {
	t.Parallel()

	err := postWebhook(context.Background(), config.Webhook{URL: deadURL(t)}, webhookMsg())
	if err == nil {
		t.Fatal("POST to a closed port succeeded")
	}

	if strings.Contains(err.Error(), "secret-token") {
		t.Errorf("error %q leaks the URL path", err)
	}

	if !isUnreachable(err) {
		t.Errorf("error %q no longer reads as unreachable after redaction", err)
	}
}
