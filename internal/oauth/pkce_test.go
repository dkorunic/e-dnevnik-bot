// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package oauth

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// A desktop client's secret ships in every binary, so it cannot prove who redeems
// an auth code; the PKCE verifier does.
// NOTE: not parallel — stubs the package-level browserOpen.
func TestGetTokenFromWebUsesPKCE(t *testing.T) {
	var verifier string

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		verifier = r.PostForm.Get("code_verifier")

		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"a","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	origBrowser := browserOpen
	defer func() { browserOpen = origBrowser }()

	browserDone := make(chan struct{})

	var consent string

	browserOpen = func(rootURL string) error {
		defer close(browserDone)

		resp, err := http.Get(rootURL) //nolint:noctx // test-only
		if err != nil {
			t.Errorf("consent page: %v", err)

			return nil
		}

		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		consent = string(body)

		driveCallback(t, rootURL, "code=c")

		return nil
	}

	cfg := &oauth2.Config{ClientID: "id", Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example.invalid/auth", TokenURL: tokenSrv.URL}}

	_, err := getTokenFromWeb(context.Background(), cfg)

	<-browserDone

	if err != nil {
		t.Fatalf("getTokenFromWeb() = %v", err)
	}

	if !strings.Contains(consent, "code_challenge=") || !strings.Contains(consent, "code_challenge_method=S256") {
		t.Errorf("consent URL has no S256 challenge: %s", consent)
	}

	if verifier == "" {
		t.Error("the token exchange sent no code_verifier")
	}
}

// A desktop client accepts any loopback port, so a busy 9080 must not end the
// flow, and the redirect has to name the port actually bound.
// NOTE: not parallel — binds the callback port and stubs browserOpen.
func TestGetTokenFromWebFallsBackWhenCallbackPortIsBusy(t *testing.T) {
	var lc net.ListenConfig

	// Already in use (a running bot, say) is the very precondition under test.
	ln, err := lc.Listen(t.Context(), "tcp", net.JoinHostPort(AuthListenAddr, strconv.Itoa(AuthListenPort)))

	switch {
	case err == nil:
		defer ln.Close() //nolint:errcheck
	case !errors.Is(err, syscall.EADDRINUSE):
		t.Fatalf("Listen() = %v", err)
	}

	origBrowser := browserOpen
	defer func() { browserOpen = origBrowser }()

	rootURLs := make(chan string, 1)
	browserOpen = func(rootURL string) error {
		rootURLs <- rootURL

		if resp, err := http.Get(rootURL + CallBackURL + "?state=wrong"); err == nil { //nolint:noctx,gosec // test loopback
			_ = resp.Body.Close()
		}

		return nil
	}

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	cfg := &oauth2.Config{Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example.invalid/auth"}}

	if _, err := getTokenFromWeb(ctx, cfg); !errors.Is(err, ErrInvalidCallbackState) {
		t.Fatalf("getTokenFromWeb() = %v, want the flow served on another port", err)
	}

	rootURL := <-rootURLs

	u, err := url.Parse(rootURL)
	if err != nil {
		t.Fatalf("url.Parse(%q) = %v", rootURL, err)
	}

	if u.Port() == strconv.Itoa(AuthListenPort) {
		t.Errorf("flow served on the busy port: %v", rootURL)
	}

	if want := rootURL + CallBackURL; cfg.RedirectURL != want {
		t.Errorf("RedirectURL = %q, want %q", cfg.RedirectURL, want)
	}
}

// A token issued to another client ID is what every user holds after an upgrade
// that ships a new one. The error has to say what to do, and stay classified as
// an auth rejection.
func TestGetClientExplainsARejectedClient(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"unauthorized_client"}`)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "token.json")
	if err := saveToken(path, &oauth2.Token{AccessToken: "a", RefreshToken: "r", Expiry: time.Now().Add(-time.Hour)}, ""); err != nil {
		t.Fatalf("saveToken() = %v", err)
	}

	_, err := GetClient(t.Context(), &oauth2.Config{Endpoint: oauth2.Endpoint{TokenURL: server.URL}}, path, false)
	if err == nil {
		t.Fatal("GetClient() = nil, want the rejection")
	}

	if !IsAuthRejected(err) {
		t.Errorf("IsAuthRejected(%v) = false", err)
	}

	if !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "re-authenticate") {
		t.Errorf("error %q does not say to delete %q and re-authenticate", err, path)
	}
}
