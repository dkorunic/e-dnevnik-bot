// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package oauth

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/rs/zerolog"
	"golang.org/x/oauth2"
)

// The Calendar API client must carry a timeout: its context only ends on
// shutdown, so a stalled server would otherwise stall the poll loop for good.
func TestGetClientHasTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if err := saveToken(path, &oauth2.Token{AccessToken: "a", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatalf("saveToken() = %v", err)
	}

	cli, err := GetClient(t.Context(), &oauth2.Config{}, path, false)
	if err != nil {
		t.Fatalf("GetClient() = %v", err)
	}

	if cli.Timeout <= 0 {
		t.Errorf("client Timeout = %v, want it bounded", cli.Timeout)
	}
}

// Token refresh goes through the same client, on its own request.
// NOTE: not parallel — swaps the package-level httpTimeout.
func TestGetClientBoundsAStalledRefresh(t *testing.T) {
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer server.Close()
	defer close(release)

	orig := httpTimeout
	httpTimeout = 50 * time.Millisecond

	t.Cleanup(func() { httpTimeout = orig })

	path := filepath.Join(t.TempDir(), "token.json")
	if err := saveToken(path, &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatalf("saveToken() = %v", err)
	}

	// Only the RED case reaches this.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	start := time.Now()

	_, err := GetClient(ctx, &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: server.URL}}, path, false)
	if err == nil {
		t.Fatal("GetClient() = nil, want a timeout")
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("refresh took %v, want it bounded by the client timeout", elapsed)
	}
}

// Without a local browser, as over SSH, the logged URL is the way in, so the
// flow has to keep serving it. A wrong state proves the server still answered.
// NOTE: not parallel — stubs the package-level browserOpen.
func TestGetTokenFromWebSurvivesBrowserFailure(t *testing.T) {
	origBrowser := browserOpen
	defer func() { browserOpen = origBrowser }()

	browserOpen = func(rootURL string) error {
		go func() {
			if resp, err := http.Get(rootURL + CallBackURL + "?state=wrong"); err == nil { //nolint:gosec,noctx // test loopback
				_ = resp.Body.Close()
			}
		}()

		return errors.New("no browser")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	_, err := getTokenFromWeb(ctx, &oauth2.Config{Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example.invalid/auth"}})
	if !errors.Is(err, ErrInvalidCallbackState) {
		t.Errorf("getTokenFromWeb() = %v, want the callback answered despite the missing browser", err)
	}
}

// The callback query carries the auth code and state; neither belongs in logs.
// NOTE: not parallel — swaps the global logger.
func TestLoggingMiddlewareOmitsQuery(t *testing.T) {
	var buf bytes.Buffer

	orig := logger.Logger
	logger.Logger = zerolog.New(&buf).Level(zerolog.DebugLevel)

	origLevel := zerolog.GlobalLevel()
	zerolog.SetGlobalLevel(zerolog.DebugLevel)

	t.Cleanup(func() {
		logger.Logger = orig

		zerolog.SetGlobalLevel(origLevel)
	})

	h := LoggingMiddleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, CallBackURL+"?code=SECRETCODE&state=SECRETSTATE", nil))

	if out := buf.String(); strings.Contains(out, "SECRET") || !strings.Contains(out, CallBackURL) {
		t.Errorf("log line = %q, want the path without the query", out)
	}
}

// pkg/browser waits for the command to exit, and a terminal browser or the first
// instance of a desktop one stays in the foreground. Neither SIGTERM nor
// AuthTimeout may depend on it.
// NOTE: not parallel — stubs the package-level browserOpen.
func TestGetTokenFromWebSurvivesBlockingBrowser(t *testing.T) {
	origBrowser := browserOpen
	defer func() { browserOpen = origBrowser }()

	release := make(chan struct{})
	defer close(release)

	browserOpen = func(string) error {
		<-release

		return nil
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)

	go func() {
		_, err := getTokenFromWeb(ctx, &oauth2.Config{Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example.invalid/auth"}})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("getTokenFromWeb() = %v, want the context's error", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("getTokenFromWeb() still blocked on the browser command after its context ended")
	}
}
