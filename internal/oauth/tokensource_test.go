// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package oauth

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// stubTokenSource returns canned tokens (and errors) in sequence, standing in
// for the real refreshing source.
type stubTokenSource struct {
	tokens []*oauth2.Token
	errs   []error
	mu     sync.Mutex
	calls  int
}

func (s *stubTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := min(s.calls, len(s.tokens)-1)
	s.calls++

	if i < len(s.errs) && s.errs[i] != nil {
		return nil, s.errs[i]
	}

	return s.tokens[i], nil
}

func (s *stubTokenSource) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.calls
}

// TestPersistingTokenSourcePersistsRotation is the point of the wrapper: Google
// rotates the access token (and sometimes the refresh token) behind the
// caller's back. If the new token is not written to disk, the next process
// start falls back to a stale token and the user is pushed through interactive
// re-auth for no reason.
func TestPersistingTokenSourcePersistsRotation(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "calendar_token.json")

	src := &stubTokenSource{
		tokens: []*oauth2.Token{
			{AccessToken: "access-1", RefreshToken: "refresh-1", Expiry: time.Now().Add(time.Hour)},
			{AccessToken: "access-2", RefreshToken: "refresh-1", Expiry: time.Now().Add(2 * time.Hour)},
		},
	}

	p := &persistingTokenSource{src: src, tokenPath: tokenPath}

	tok, err := p.Token()
	if err != nil {
		t.Fatalf("Token() = %v, want nil", err)
	}

	if tok.AccessToken != "access-1" {
		t.Fatalf("Token() AccessToken = %q, want access-1", tok.AccessToken)
	}

	onDisk, err := tokenFromFile(tokenPath)
	if err != nil {
		t.Fatalf("token was not persisted on first use: %v", err)
	}

	if onDisk.AccessToken != "access-1" {
		t.Errorf("persisted AccessToken = %q, want access-1", onDisk.AccessToken)
	}

	// Rotation: a changed access token must be written through.
	if _, err := p.Token(); err != nil {
		t.Fatalf("Token() second call = %v, want nil", err)
	}

	onDisk, err = tokenFromFile(tokenPath)
	if err != nil {
		t.Fatalf("tokenFromFile after rotation: %v", err)
	}

	if onDisk.AccessToken != "access-2" {
		t.Errorf("persisted AccessToken = %q, want access-2 — a rotated token must be written through", onDisk.AccessToken)
	}
}

// TestPersistingTokenSourceSkipsUnchangedWrites: an unchanged token must not
// rewrite the file. Every write is an atomic create+rename, so rewriting on
// each of the many Calendar API calls per cycle is pure churn.
func TestPersistingTokenSourceSkipsUnchangedWrites(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "calendar_token.json")

	same := &oauth2.Token{AccessToken: "access-1", RefreshToken: "refresh-1", Expiry: time.Now().Add(time.Hour)}
	p := &persistingTokenSource{src: &stubTokenSource{tokens: []*oauth2.Token{same}}, tokenPath: tokenPath}

	if _, err := p.Token(); err != nil {
		t.Fatalf("Token() = %v, want nil", err)
	}

	// Make a later rewrite detectable regardless of filesystem timestamp
	// granularity by replacing the contents with a marker.
	if err := os.WriteFile(tokenPath, []byte(`{"access_token":"marker"}`), DefaultPerms); err != nil {
		t.Fatal(err)
	}

	for range 5 {
		if _, err := p.Token(); err != nil {
			t.Fatalf("Token() = %v, want nil", err)
		}
	}

	data, err := os.ReadFile(tokenPath)
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != `{"access_token":"marker"}` {
		t.Error("an unchanged token was rewritten to disk; the changed check must suppress redundant atomic writes")
	}
}

// TestPersistingTokenSourceRefreshTokenRotation: Google occasionally rotates
// the refresh token while leaving the access token alone. Keying the write on
// the access token alone would drop the new refresh token and force
// re-authentication once the old one expires.
func TestPersistingTokenSourceRefreshTokenRotation(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "calendar_token.json")

	src := &stubTokenSource{
		tokens: []*oauth2.Token{
			{AccessToken: "same-access", RefreshToken: "refresh-1", Expiry: time.Now().Add(time.Hour)},
			{AccessToken: "same-access", RefreshToken: "refresh-2", Expiry: time.Now().Add(time.Hour)},
		},
	}

	p := &persistingTokenSource{src: src, tokenPath: tokenPath}

	if _, err := p.Token(); err != nil {
		t.Fatalf("Token() = %v, want nil", err)
	}

	if _, err := p.Token(); err != nil {
		t.Fatalf("Token() = %v, want nil", err)
	}

	onDisk, err := tokenFromFile(tokenPath)
	if err != nil {
		t.Fatalf("tokenFromFile: %v", err)
	}

	if onDisk.RefreshToken != "refresh-2" {
		t.Errorf("persisted RefreshToken = %q, want refresh-2 — rotation must be detected on the refresh token too",
			onDisk.RefreshToken)
	}
}

// TestPersistingTokenSourceReturnsSourceError: a refresh failure must surface
// to the caller rather than being swallowed into a nil token, and nothing must
// be written to disk.
func TestPersistingTokenSourceReturnsSourceError(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "calendar_token.json")
	wantErr := errors.New("oauth2: cannot fetch token")

	p := &persistingTokenSource{
		src: &stubTokenSource{
			tokens: []*oauth2.Token{nil},
			errs:   []error{wantErr},
		},
		tokenPath: tokenPath,
	}

	tok, err := p.Token()
	if !errors.Is(err, wantErr) {
		t.Fatalf("Token() = %v, want %v", err, wantErr)
	}

	if tok != nil {
		t.Errorf("Token() returned %+v alongside an error, want nil", tok)
	}

	if _, err := os.Stat(tokenPath); !os.IsNotExist(err) {
		t.Error("a failed refresh wrote a token file")
	}
}

// TestPersistingTokenSourceInvalidGrantWarnsOnce: a revoked grant fails every
// subsequent API call, so the operator-facing warning is latched behind a
// sync.Once — otherwise it would flood the log once per call, per cycle. The
// error itself must still be returned every time.
func TestPersistingTokenSourceInvalidGrantWarnsOnce(t *testing.T) {
	t.Parallel()

	revoked := &oauth2.RetrieveError{
		ErrorCode: "invalid_grant",
		Body:      []byte(`{"error":"invalid_grant"}`),
	}

	if !IsInvalidGrant(revoked) {
		t.Skip("fixture is not recognised as an invalid_grant; IsInvalidGrant semantics changed")
	}

	src := &stubTokenSource{
		tokens: []*oauth2.Token{nil},
		errs:   []error{revoked},
	}
	p := &persistingTokenSource{src: src, tokenPath: filepath.Join(t.TempDir(), "calendar_token.json")}

	for i := range 3 {
		if _, err := p.Token(); !errors.Is(err, revoked) {
			t.Fatalf("call %d: Token() = %v, want the revoked-grant error every time", i, err)
		}
	}

	if got := src.callCount(); got != 3 {
		t.Errorf("underlying source called %d times, want 3 — the warn latch must not suppress the refresh attempt", got)
	}
}

// TestPersistingTokenSourceConcurrentRefresh: the mutex is held across
// saveToken so concurrent refreshes cannot reorder the atomic rename and leave
// an older token on disk. Meaningful under -race.
func TestPersistingTokenSourceConcurrentRefresh(t *testing.T) {
	t.Parallel()

	tokenPath := filepath.Join(t.TempDir(), "calendar_token.json")

	src := &stubTokenSource{
		tokens: []*oauth2.Token{{AccessToken: "access-1", RefreshToken: "refresh-1", Expiry: time.Now().Add(time.Hour)}},
	}
	p := &persistingTokenSource{src: src, tokenPath: tokenPath}

	var wg sync.WaitGroup

	for range 16 {
		wg.Go(func() {
			if _, err := p.Token(); err != nil {
				t.Errorf("concurrent Token() = %v, want nil", err)
			}
		})
	}

	wg.Wait()

	onDisk, err := tokenFromFile(tokenPath)
	if err != nil {
		t.Fatalf("tokenFromFile: %v", err)
	}

	if onDisk.AccessToken != "access-1" {
		t.Errorf("persisted AccessToken = %q, want access-1", onDisk.AccessToken)
	}
}

// TestPersistingTokenSourceTolerateUnwritablePath: persistence is best-effort.
// The caller must still get a usable token when the file cannot be written —
// losing cross-restart reuse is acceptable, failing the API call is not.
func TestPersistingTokenSourceTolerateUnwritablePath(t *testing.T) {
	t.Parallel()

	// A path under a non-existent directory cannot be written.
	tokenPath := filepath.Join(t.TempDir(), "no-such-dir", "calendar_token.json")

	p := &persistingTokenSource{
		src: &stubTokenSource{
			tokens: []*oauth2.Token{{AccessToken: "access-1", Expiry: time.Now().Add(time.Hour)}},
		},
		tokenPath: tokenPath,
	}

	tok, err := p.Token()
	if err != nil {
		t.Fatalf("Token() = %v, want nil — persistence failure must not fail the call", err)
	}

	if tok.AccessToken != "access-1" {
		t.Errorf("Token() AccessToken = %q, want access-1", tok.AccessToken)
	}
}
