// SPDX-FileCopyrightText: 2023 Dinko Korunic
// SPDX-License-Identifier: MIT

package oauth

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/renameio/v2/maybe"
	"github.com/google/uuid"
	"github.com/pkg/browser"
	"golang.org/x/oauth2"
)

const (
	AuthTimeout       = 300 * time.Second
	AuthListenAddr    = "localhost"
	AuthListenPort    = 9080
	AuthScheme        = "http://"
	CallBackURL       = "/callback"
	DefaultPerms      = 0o600
	ReadTimeout       = 5 * time.Second
	WriteTimeout      = 5 * time.Second
	IdleTimeout       = 60 * time.Second
	ReadHeaderTimeout = 10 * time.Second
)

var (
	ErrOAuthUUID            = errors.New("unable to generate UUID")
	ErrOAuthHTTPServer      = errors.New("unable to start HTTP server")
	ErrOAuthBrowser         = errors.New("unable to open system browser")
	ErrOAuthTimeout         = errors.New("timeout while waiting for authentication to finish")
	ErrOAuthTokenFetch      = errors.New("unable to retrieve token from Google API")
	ErrOAuthTokenSave       = errors.New("unable to save token to file")
	ErrOAuthTokenEncode     = errors.New("unable to encode OAuth token to JSON")
	ErrInvalidCallbackState = errors.New("invalid OAuth callback state")
)

//go:embed templates/*html assets/*ico
var contentFS embed.FS

// persistingTokenSource persists refreshed OAuth tokens to disk so restarts survive token rotation.
type persistingTokenSource struct {
	src       oauth2.TokenSource
	last      *oauth2.Token
	tokenPath string
	mu        sync.Mutex
}

// Token returns a cached/refreshed token, and persists it to tokenPath on
// disk whenever the AccessToken (or RefreshToken) differs from the last
// token we observed. A persistence error is logged rather than returned —
// the caller still gets a usable token, we just lose the ability to reuse
// it across restarts until the next refresh.
func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		return nil, err
	}

	// Hold across saveToken so concurrent refreshes can't reorder the atomic rename.
	p.mu.Lock()
	defer p.mu.Unlock()

	changed := p.last == nil ||
		p.last.AccessToken != tok.AccessToken ||
		p.last.RefreshToken != tok.RefreshToken
	if changed {
		p.last = tok

		if serr := saveToken(p.tokenPath, tok); serr != nil {
			logger.Warn().Msgf("Unable to persist refreshed OAuth token: %v", serr)
		}
	}

	return tok, nil
}

// GetClient returns an OAuth2 HTTP client, loading the token from tokenPath —
// or running the interactive web flow if none exists — and persisting any
// rotated token so restarts survive refresh-token rotation.
func GetClient(ctx context.Context, config *oauth2.Config, tokenPath string) (*http.Client, error) {
	tok, err := tokenFromFile(tokenPath)
	saveToFile := false

	//nolint:nestif
	if err == nil {
		if !tok.Valid() {
			src := config.TokenSource(ctx, tok)

			newTok, err := src.Token()
			if err != nil {
				return nil, err
			}

			// Persist rotated refresh tokens; otherwise startup bricks after old one expires.
			if newTok.AccessToken != tok.AccessToken || newTok.RefreshToken != tok.RefreshToken {
				saveToFile = true
				tok = newTok
			}
		}
	} else {
		tok, err = getTokenFromWeb(ctx, config)
		if err != nil {
			return nil, err
		}

		saveToFile = true
	}

	if saveToFile {
		if err = saveToken(tokenPath, tok); err != nil {
			return nil, err
		}
	}

	// Persist refreshes so restarts survive token rotation.
	ts := &persistingTokenSource{
		src:       config.TokenSource(ctx, tok),
		tokenPath: tokenPath,
		last:      tok,
	}

	return oauth2.NewClient(ctx, ts), nil
}

// getTokenFromWeb runs the interactive consent flow: it serves a loopback
// callback, opens the browser, and exchanges the returned code for a token.
// State is CSRF-checked and the wait is bounded by AuthTimeout.
func getTokenFromWeb(ctx context.Context, config *oauth2.Config) (*oauth2.Token, error) {
	// Random state for CSRF protection on the OAuth callback.
	authReqState, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOAuthUUID, err)
	}

	tokChan := make(chan string, 1)

	var once sync.Once

	// Bind first so authURL carries the actual port; loopback is required by Google.
	authListenHost := net.JoinHostPort(AuthListenAddr, strconv.Itoa(AuthListenPort))

	listener, err := net.Listen("tcp", authListenHost)
	if err != nil {
		logger.Warn().Msgf("Preferred OAuth callback port %v unavailable (%v), falling back to an ephemeral port",
			AuthListenPort, err)

		listener, err = net.Listen("tcp", net.JoinHostPort(AuthListenAddr, "0"))
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrOAuthHTTPServer, err)
		}
	}

	authURL := AuthScheme + listener.Addr().String()

	config.RedirectURL = authURL + CallBackURL

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(LoggingMiddleware)

	t := template.Must(template.ParseFS(contentFS, "templates/*html"))

	s := http.Server{
		ReadTimeout:       ReadTimeout,
		WriteTimeout:      WriteTimeout,
		IdleTimeout:       IdleTimeout,
		ReadHeaderTimeout: ReadHeaderTimeout,
		Addr:              listener.Addr().String(),
		Handler:           r,
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), WriteTimeout)
		defer cancel()
		_ = s.Shutdown(shutdownCtx)
	}()

	authCodeURL := config.AuthCodeURL(authReqState.String(), oauth2.AccessTypeOffline, oauth2.ApprovalForce)

	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		if err := t.ExecuteTemplate(w, "index.html", map[string]any{
			"authURL": authCodeURL,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})

	r.Get(CallBackURL, func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		// Constant-time compare to avoid timing leaks on malformed callbacks.
		expectedState := authReqState.String()
		if receivedState := req.URL.Query().Get("state"); subtle.ConstantTimeCompare([]byte(receivedState), []byte(expectedState)) != 1 {
			w.WriteHeader(http.StatusBadRequest)

			if err := t.ExecuteTemplate(w, "failure.html", map[string]any{"error": ErrInvalidCallbackState}); err != nil {
				logger.Error().Msgf("template execution failed: %v", err)
			}

			once.Do(func() { close(tokChan) })

			return
		}

		once.Do(func() {
			tokChan <- req.URL.Query().Get("code")
			close(tokChan)
		})

		if err := t.ExecuteTemplate(w, "success.html", map[string]any{}); err != nil {
			logger.Error().Msgf("template execution failed: %v", err)
		}
	})

	r.Get("/favicon.ico", func(w http.ResponseWriter, req *http.Request) {
		http.ServeFileFS(w, req, contentFS, "assets/favicon.ico")
	})

	go func() {
		logger.Debug().Msgf("starting HTTP listener on: %v", s.Addr)

		// Serve on pre-bound listener; ListenAndServe would race by re-binding.
		if err := s.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal().Msgf("%v: %v", ErrOAuthHTTPServer, err)
		}
	}()

	logger.Info().Msgf("Opening local Web server through system browser: %v", authURL)

	if err := browser.OpenURL(authURL); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOAuthBrowser, err)
	}

	var authCode string

	authTimer := time.NewTimer(AuthTimeout)
	defer authTimer.Stop()

	select {
	case authCode = <-tokChan:
		authTimer.Stop()
	case <-authTimer.C:
		return nil, ErrOAuthTimeout
	case <-ctx.Done():
		// SIGTERM during consent must abort promptly, not block for AuthTimeout.
		return nil, ctx.Err()
	}

	// Empty code indicates a state-mismatch callback.
	if authCode == "" {
		return nil, ErrInvalidCallbackState
	}

	tok, err := config.Exchange(ctx, authCode)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOAuthTokenFetch, err)
	}

	return tok, nil
}

// tokenFromFile reads and JSON-decodes an OAuth2 token from tokenPath.
func tokenFromFile(tokenPath string) (*oauth2.Token, error) {
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, err
	}

	tok := &oauth2.Token{}

	err = json.NewDecoder(bytes.NewBuffer(b)).Decode(tok)

	return tok, err
}

// saveToken atomically writes token to tokenPath as JSON with DefaultPerms.
func saveToken(tokenPath string, token *oauth2.Token) error {
	buf := new(bytes.Buffer)

	err := json.NewEncoder(buf).Encode(token)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrOAuthTokenEncode, err)
	}

	if err = maybe.WriteFile(tokenPath, buf.Bytes(), DefaultPerms); err != nil {
		return fmt.Errorf("%w: %w", ErrOAuthTokenSave, err)
	}

	return nil
}

// LoggingMiddleware logs method, URI, status, client IP, and duration per request.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		reqDuration := time.Since(startTime)

		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)

		logger.Debug().Msgf("OAuth HTTP server request: method: %v, uri: %v, status: %v, client ip: %v, duration: %v",
			r.Method, r.URL.RequestURI(), ww.Status(), clientIP, reqDuration)
	})
}
