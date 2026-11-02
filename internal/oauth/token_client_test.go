// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package oauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestTokenClientID(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	marked := filepath.Join(dir, "marked.json")
	if err := saveToken(marked, &oauth2.Token{AccessToken: "a", RefreshToken: "r"}, "desktop-id"); err != nil {
		t.Fatalf("saveToken() = %v", err)
	}

	id, err := TokenClientID(marked)
	if err != nil || id != "desktop-id" {
		t.Errorf("marked: TokenClientID() = %q, %v, want desktop-id", id, err)
	}

	// The marker sits beside the token's own fields, which must still decode.
	tok, err := tokenFromFile(marked)
	if err != nil || tok.AccessToken != "a" || tok.RefreshToken != "r" {
		t.Errorf("tokenFromFile() = %+v, %v, want the token intact", tok, err)
	}

	// What every existing install has on disk, byte for byte in shape.
	unmarked := filepath.Join(dir, "unmarked.json")
	if err := os.WriteFile(unmarked, []byte(`{"access_token":"a","token_type":"Bearer","refresh_token":"r","expiry":"2099-01-01T00:00:00Z"}`), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}

	if id, err := TokenClientID(unmarked); err != nil || id != "" {
		t.Errorf("unmarked: TokenClientID() = %q, %v, want no marker and no error", id, err)
	}

	if _, err := TokenClientID(filepath.Join(dir, "absent.json")); err == nil {
		t.Error("absent: TokenClientID() = nil error")
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("garbage {"), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}

	if _, err := TokenClientID(corrupt); err == nil {
		t.Error("corrupt: TokenClientID() = nil error")
	}
}

// A refresh rewrites the file. Losing the marker there would hand a desktop token
// to the legacy client on the next start, which Google rejects.
func TestRefreshKeepsTheClientMarker(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"fresh","token_type":"Bearer","expires_in":3600}`)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "token.json")
	if err := saveToken(path, &oauth2.Token{AccessToken: "stale", RefreshToken: "r", Expiry: time.Now().Add(-time.Hour)}, "desktop-id"); err != nil {
		t.Fatalf("saveToken() = %v", err)
	}

	cfg := &oauth2.Config{ClientID: "desktop-id", Endpoint: oauth2.Endpoint{TokenURL: server.URL}}

	if _, err := GetClient(t.Context(), cfg, path, false); err != nil {
		t.Fatalf("GetClient() = %v", err)
	}

	tok, err := tokenFromFile(path)
	if err != nil || tok.AccessToken != "fresh" {
		t.Fatalf("tokenFromFile() = %+v, %v, want the refreshed token", tok, err)
	}

	if id, err := TokenClientID(path); err != nil || id != "desktop-id" {
		t.Errorf("TokenClientID() = %q, %v, want the marker kept", id, err)
	}
}

// An existing install's first refresh records which client it belongs to, which
// is the legacy one GetClient was given. It keeps working either way.
func TestPersistingTokenSourceWritesItsClient(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "token.json")

	p := &persistingTokenSource{
		src:       oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "fresh"}),
		tokenPath: path,
		clientID:  "legacy-id",
	}

	if _, err := p.Token(); err != nil {
		t.Fatalf("Token() = %v", err)
	}

	if id, err := TokenClientID(path); err != nil || id != "legacy-id" {
		t.Errorf("TokenClientID() = %q, %v, want legacy-id", id, err)
	}
}

// A new setup's token has to say which client issued it from the very first save.
// NOTE: not parallel — stubs the package-level browserOpen.
func TestConsentMarksTheNewToken(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"a","refresh_token":"r","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	origBrowser := browserOpen
	defer func() { browserOpen = origBrowser }()

	browserDone := make(chan struct{})

	browserOpen = func(rootURL string) error {
		defer close(browserDone)

		driveCallback(t, rootURL, "code=c")

		return nil
	}

	path := filepath.Join(t.TempDir(), "token.json")
	cfg := &oauth2.Config{ClientID: "desktop-id", Endpoint: oauth2.Endpoint{AuthURL: "https://accounts.example.invalid/auth", TokenURL: tokenSrv.URL}}

	_, err := GetClient(context.Background(), cfg, path, true)

	<-browserDone

	if err != nil {
		t.Fatalf("GetClient() = %v", err)
	}

	if id, err := TokenClientID(path); err != nil || id != "desktop-id" {
		t.Errorf("TokenClientID() = %q, %v, want desktop-id", id, err)
	}
}
