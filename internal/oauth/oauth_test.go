// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package oauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestTokenFileOperations(t *testing.T) {
	t.Parallel()

	tmpfile, err := os.CreateTemp("", "test-token.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	expectedToken := &oauth2.Token{
		AccessToken:  "test-access-token",
		TokenType:    "Bearer",
		RefreshToken: "test-refresh-token",
		Expiry:       time.Now().Add(1 * time.Hour).Round(time.Second),
	}

	if err := saveToken(tmpfile.Name(), expectedToken); err != nil {
		t.Fatalf("saveToken failed: %v", err)
	}

	actualToken, err := tokenFromFile(tmpfile.Name())
	if err != nil {
		t.Fatalf("tokenFromFile failed: %v", err)
	}

	if !reflect.DeepEqual(expectedToken, actualToken) {
		t.Errorf("token mismatch: expected %v, got %v", expectedToken, actualToken)
	}
}

func TestTokenFromFileNonExistent(t *testing.T) {
	t.Parallel()
	_, err := tokenFromFile("/non/existent/path/to/token.json")
	if err == nil {
		t.Error("tokenFromFile() should fail for non-existent file")
	}
}

func TestTokenFromFileInvalidJSON(t *testing.T) {
	t.Parallel()
	tmpfile, err := os.CreateTemp("", "test-invalid-token-*.json")
	if err != nil {
		t.Fatal(err)
	}

	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte("not valid json {")); err != nil {
		t.Fatal(err)
	}

	if err := tmpfile.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = tokenFromFile(tmpfile.Name())
	if err == nil {
		t.Error("tokenFromFile() should fail for invalid JSON")
	}
}

func TestSaveTokenToUnwritablePath(t *testing.T) {
	t.Parallel()

	err := saveToken("/non/existent/dir/token.json", &oauth2.Token{})
	if err == nil {
		t.Error("saveToken() should fail when destination directory does not exist")
	}
}

// stateRe extracts the state query param from the rendered index page (the
// href is HTML-escaped, so match the raw "state=<uuid>" fragment).
var stateRe = regexp.MustCompile(`state=([0-9a-fA-F-]{36})`)

// driveCallback fetches the consent page at rootURL, extracts the OAuth state,
// and issues the callback with the given extra query ("error=..." or "code=...").
// Returns the callback response body for assertions.
func driveCallback(t *testing.T, rootURL, extraQuery string) string {
	t.Helper()

	resp, err := http.Get(rootURL) //nolint:noctx // test-only
	if err != nil {
		t.Fatalf("failed to fetch consent page: %v", err)
	}

	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()

	if err != nil {
		t.Fatalf("failed to read consent page: %v", err)
	}

	m := stateRe.FindSubmatch(body)
	if m == nil {
		t.Fatalf("no state parameter found in consent page: %s", body)
	}

	cbResp, err := http.Get(rootURL + CallBackURL + "?state=" + string(m[1]) + "&" + extraQuery) //nolint:noctx // test-only
	if err != nil {
		t.Fatalf("failed to drive callback: %v", err)
	}
	defer cbResp.Body.Close()

	cbBody, err := io.ReadAll(cbResp.Body)
	if err != nil {
		t.Fatalf("failed to read callback page: %v", err)
	}

	return string(cbBody)
}

// TestGetTokenFromWebDenied verifies that a consent denial (error query param
// on the callback) surfaces ErrOAuthDenied and renders the failure page rather
// than a false success.
// NOTE: not parallel — stubs the package-level browserOpen.
func TestGetTokenFromWebDenied(t *testing.T) {
	origBrowser := browserOpen
	defer func() { browserOpen = origBrowser }()

	var cbPage string

	browserOpen = func(url string) error {
		cbPage = driveCallback(t, url, "error=access_denied")

		return nil
	}

	cfg := &oauth2.Config{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.example.invalid/auth",
			TokenURL: "https://accounts.example.invalid/token",
		},
	}

	_, err := getTokenFromWeb(context.Background(), cfg)
	if !errors.Is(err, ErrOAuthDenied) {
		t.Fatalf("getTokenFromWeb() error = %v, want ErrOAuthDenied", err)
	}

	if !regexp.MustCompile(`access_denied`).MatchString(cbPage) {
		t.Errorf("denial should render the failure page with the cause, got: %s", cbPage)
	}
}

// TestGetTokenFromWebSuccess drives the full loopback flow: consent page →
// state extraction → callback with code → token exchange against a stub
// endpoint.
// NOTE: not parallel — stubs the package-level browserOpen.
func TestGetTokenFromWebSuccess(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"stub-access-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	origBrowser := browserOpen
	defer func() { browserOpen = origBrowser }()

	var cbPage string

	browserOpen = func(url string) error {
		cbPage = driveCallback(t, url, "code=stub-auth-code")

		return nil
	}

	cfg := &oauth2.Config{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.example.invalid/auth",
			TokenURL: tokenSrv.URL,
		},
	}

	tok, err := getTokenFromWeb(context.Background(), cfg)
	if err != nil {
		t.Fatalf("getTokenFromWeb() failed: %v", err)
	}

	if tok.AccessToken != "stub-access-token" {
		t.Errorf("unexpected access token: %q", tok.AccessToken)
	}

	if !regexp.MustCompile(`Authentication complete`).MatchString(cbPage) {
		t.Errorf("valid callback should render the success page, got: %s", cbPage)
	}
}

// TestValidateTokenFile verifies startup token-file validation: a decodable
// token passes, while a corrupt or missing file fails fast.
func TestValidateTokenFile(t *testing.T) {
	t.Parallel()

	if err := ValidateTokenFile("/non/existent/token.json"); err == nil {
		t.Error("ValidateTokenFile() should fail for a missing file")
	}

	tmpfile, err := os.CreateTemp("", "test-invalid-token-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	if _, err := tmpfile.Write([]byte("garbage {")); err != nil {
		t.Fatal(err)
	}
	tmpfile.Close()

	if err := ValidateTokenFile(tmpfile.Name()); err == nil {
		t.Error("ValidateTokenFile() should fail for a corrupt file")
	}

	valid := &oauth2.Token{AccessToken: "x", TokenType: "Bearer", RefreshToken: "y"}
	if err := saveToken(tmpfile.Name(), valid); err != nil {
		t.Fatalf("saveToken failed: %v", err)
	}

	if err := ValidateTokenFile(tmpfile.Name()); err != nil {
		t.Errorf("ValidateTokenFile() failed for a valid token: %v", err)
	}
}

// TestIsInvalidGrant verifies invalid_grant detection through error wrapping.
func TestIsInvalidGrant(t *testing.T) {
	t.Parallel()

	if IsInvalidGrant(nil) {
		t.Error("IsInvalidGrant(nil) should be false")
	}

	if IsInvalidGrant(errors.New("invalid_grant")) {
		t.Error("plain string match should not count as invalid_grant")
	}

	re := &oauth2.RetrieveError{ErrorCode: "invalid_grant"}
	if !IsInvalidGrant(fmt.Errorf("wrapped: %w", re)) {
		t.Error("wrapped RetrieveError{invalid_grant} should be detected")
	}

	re.ErrorCode = "temporarily_unavailable"
	if IsInvalidGrant(re) {
		t.Error("non-invalid_grant RetrieveError should not be detected")
	}
}

// TestGetClientInvalidGrant verifies a revoked refresh token surfaces as
// ErrOAuthGrantRevoked with an actionable recovery path, not a bare API error.
func TestGetClientInvalidGrant(t *testing.T) {
	t.Parallel()

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"Token has been revoked."}`)
	}))
	defer tokenSrv.Close()

	// Expired token forces the refresh path in GetClient.
	tmpfile, err := os.CreateTemp("", "test-expired-token-*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	expired := &oauth2.Token{
		AccessToken:  "old-access",
		TokenType:    "Bearer",
		RefreshToken: "revoked-refresh",
		Expiry:       time.Now().Add(-time.Hour),
	}
	if err := saveToken(tmpfile.Name(), expired); err != nil {
		t.Fatalf("saveToken failed: %v", err)
	}

	cfg := &oauth2.Config{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.example.invalid/auth",
			TokenURL: tokenSrv.URL,
		},
	}

	_, err = GetClient(context.Background(), cfg, tmpfile.Name())
	if !errors.Is(err, ErrOAuthGrantRevoked) {
		t.Fatalf("GetClient() error = %v, want ErrOAuthGrantRevoked", err)
	}
}

func TestLoggingMiddleware(t *testing.T) {
	t.Parallel()
	nextCalled := false

	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalled = true
		w.WriteHeader(http.StatusOK)
	})

	handler := LoggingMiddleware(next)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if !nextCalled {
		t.Error("LoggingMiddleware did not call next handler")
	}

	if w.Code != http.StatusOK {
		t.Errorf("LoggingMiddleware returned status %d, want %d", w.Code, http.StatusOK)
	}
}

// TestSaveTokenPermissions: the file holds the OAuth refresh token, a long-lived
// credential for the user's calendar, so it must never be group- or
// world-readable. maybe.WriteFile creates the file, so the mode argument is the
// only thing between the token and a 0644 default — a one-character change no
// behavioural test would otherwise notice.
func TestSaveTokenPermissions(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}

	tokenPath := filepath.Join(t.TempDir(), "calendar_token.json")

	if err := saveToken(tokenPath, &oauth2.Token{AccessToken: "secret"}); err != nil {
		t.Fatalf("saveToken() failed: %v", err)
	}

	fi, err := os.Stat(tokenPath)
	if err != nil {
		t.Fatalf("stat after saveToken() failed: %v", err)
	}

	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("token file mode = %#o, want %#o — the OAuth refresh token must not be readable by group or other",
			perm, 0o600)
	}
}

// TestGetTokenFromWebRejectsForgedState covers the CSRF guard on the callback.
// It listens on loopback, so any local process — or a web page issuing a
// cross-origin GET to 127.0.0.1 — can reach it. Without the state check the
// first caller wins: the bot exchanges the attacker's authorization code and
// persists a token for the attacker's account.
//
// The comparison is constant-time to avoid leaking the expected state; the
// property here is only that a mismatch is refused.
// NOTE: not parallel — stubs the package-level browserOpen.
func TestGetTokenFromWebRejectsForgedState(t *testing.T) {
	origBrowser := browserOpen
	defer func() { browserOpen = origBrowser }()

	var cbPage string

	browserOpen = func(rootURL string) error {
		// Skip the consent page entirely: hit the callback with a state the
		// server never issued, as a third party would have to.
		resp, err := http.Get(rootURL + CallBackURL + "?state=forged-by-attacker&code=attacker-auth-code") //nolint:noctx // test-only
		if err != nil {
			t.Errorf("failed to drive callback: %v", err)

			return nil
		}

		defer resp.Body.Close()

		body, _ := io.ReadAll(resp.Body)
		cbPage = string(body)

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("forged callback status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
		}

		return nil
	}

	cfg := &oauth2.Config{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.example.invalid/auth",
			TokenURL: "https://accounts.example.invalid/token",
		},
	}

	_, err := getTokenFromWeb(context.Background(), cfg)
	if !errors.Is(err, ErrInvalidCallbackState) {
		t.Fatalf("getTokenFromWeb() error = %v, want ErrInvalidCallbackState; a callback carrying a state the server never issued must never be exchanged", err)
	}

	if regexp.MustCompile(`Authentication complete`).MatchString(cbPage) {
		t.Error("a forged callback rendered the success page")
	}
}

// TestGetTokenFromWebRejectsMissingCode: a callback that passes the state check
// but carries neither code nor error. Google does not produce this; a truncated
// redirect or hand-typed URL does. Without the guard the empty code reaches
// config.Exchange and fails with an opaque provider error, after a round trip,
// instead of being diagnosed locally.
// NOTE: not parallel — stubs the package-level browserOpen.
func TestGetTokenFromWebRejectsMissingCode(t *testing.T) {
	origBrowser := browserOpen
	defer func() { browserOpen = origBrowser }()

	browserOpen = func(rootURL string) error {
		// Correct state (driveCallback reads it from the consent page), but no
		// code and no error parameter.
		driveCallback(t, rootURL, "")

		return nil
	}

	cfg := &oauth2.Config{
		ClientID:     "test-client",
		ClientSecret: "test-secret",
		Endpoint: oauth2.Endpoint{
			AuthURL:  "https://accounts.example.invalid/auth",
			TokenURL: "https://accounts.example.invalid/token",
		},
	}

	_, err := getTokenFromWeb(context.Background(), cfg)
	if !errors.Is(err, ErrInvalidCallbackState) {
		t.Fatalf("getTokenFromWeb() error = %v, want ErrInvalidCallbackState for a callback with no code", err)
	}
}
