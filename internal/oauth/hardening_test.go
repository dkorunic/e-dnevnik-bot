// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package oauth

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	if err := saveToken(path, &oauth2.Token{AccessToken: "a", Expiry: time.Now().Add(time.Hour)}, ""); err != nil {
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
	if err := saveToken(path, &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(-time.Hour)}, ""); err != nil {
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

// The command is pasted on another machine, so it must name this host as the user
// reached it: the session's address and sshd port, then the sudo caller's login.
// NOTE: not parallel — sets the environment.
func TestSSHTunnelCommand(t *testing.T) {
	hostname, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}

	tests := []struct {
		name, addr, sshConn, user, sudoUser, want string
	}{
		{
			name: "session on default port", addr: "127.0.0.1:46509",
			sshConn: "198.51.100.7 50000 203.0.113.5 22", user: "alice",
			want: "ssh -L 46509:127.0.0.1:46509 alice@203.0.113.5",
		},
		{
			name: "session on custom port", addr: "127.0.0.1:9080",
			sshConn: "198.51.100.7 50000 203.0.113.5 2222", user: "alice",
			want: "ssh -p 2222 -L 9080:127.0.0.1:9080 alice@203.0.113.5",
		},
		{
			name: "sudo keeps the login user", addr: "127.0.0.1:9080",
			sshConn: "198.51.100.7 50000 203.0.113.5 22", user: "ednevnik", sudoUser: "alice",
			want: "ssh -L 9080:127.0.0.1:9080 alice@203.0.113.5",
		},
		{
			name: "no session falls back to hostname", addr: "127.0.0.1:9080", user: "alice",
			want: "ssh -L 9080:127.0.0.1:9080 alice@" + hostname,
		},
		{
			name: "loopback IPv6 stays bracketed", addr: "[::1]:9080", user: "alice",
			sshConn: "198.51.100.7 50000 203.0.113.5 22",
			want:    "ssh -L 9080:[::1]:9080 alice@203.0.113.5",
		},
		{
			name: "no user lets ssh choose", addr: "127.0.0.1:9080",
			sshConn: "198.51.100.7 50000 203.0.113.5 22",
			want:    "ssh -L 9080:127.0.0.1:9080 203.0.113.5",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SSH_CONNECTION", tt.sshConn)
			t.Setenv("USER", tt.user)
			t.Setenv("SUDO_USER", tt.sudoUser)

			if got := sshTunnelCommand(tt.addr); got != tt.want {
				t.Errorf("sshTunnelCommand(%q) = %q, want %q", tt.addr, got, tt.want)
			}
		})
	}
}

// lineWriter hands each log line to a test without sharing a buffer across goroutines.
type lineWriter chan string

func (w lineWriter) Write(p []byte) (int, error) {
	w <- string(p)

	return len(p), nil
}

// A failed browser launch must log the tunnel for the port actually bound, which
// differs from AuthListenPort whenever the fallback ran.
// NOTE: not parallel — stubs browserOpen, swaps the global logger, sets the environment.
func TestGetTokenFromWebLogsTunnelForBoundPort(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "198.51.100.7 50000 203.0.113.5 22")
	t.Setenv("USER", "alice")
	t.Setenv("SUDO_USER", "")

	// Forces the ephemeral fallback, so a hardcoded AuthListenPort cannot pass.
	// An error means something else already holds it, which forces the same.
	if busy, err := net.Listen("tcp", net.JoinHostPort(AuthListenAddr, strconv.Itoa(AuthListenPort))); err == nil { //nolint:noctx // test-only
		defer busy.Close()
	}

	origBrowser := browserOpen
	defer func() { browserOpen = origBrowser }()

	opened := make(chan string, 1)

	browserOpen = func(rootURL string) error {
		opened <- rootURL

		return errors.New("no browser")
	}

	lines := make(lineWriter, 16)

	orig := logger.Logger
	logger.Logger = zerolog.New(lines).Level(zerolog.DebugLevel)

	defer func() { logger.Logger = orig }()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	done := make(chan struct{})

	go func() {
		defer close(done)

		_, _ = getTokenFromWeb(ctx, &oauth2.Config{Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example.invalid/auth"}})
	}()

	rootURL := <-opened

	u, err := url.Parse(rootURL)
	if err != nil {
		t.Fatalf("browser URL %q: %v", rootURL, err)
	}

	want := "ssh -L " + u.Port() + ":" + u.Host + " alice@203.0.113.5"

	found := false

	for !found {
		select {
		case line := <-lines:
			found = strings.Contains(line, want)
		case <-ctx.Done():
			t.Errorf("no log line contains %q", want)

			found = true
		}
	}

	cancel()
	<-done
}
