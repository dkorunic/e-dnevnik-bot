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
	ErrOAuthDenied          = errors.New("OAuth consent flow returned an error")
	ErrOAuthGrantRevoked    = errors.New("OAuth grant revoked or expired")
)

// browserOpen is a test seam; production uses browser.OpenURL.
var browserOpen = browser.OpenURL

//go:embed templates/*html assets/*ico
var contentFS embed.FS

// persistingTokenSource writes refreshed tokens to disk, so a restart survives
// rotation.
type persistingTokenSource struct {
	src       oauth2.TokenSource
	last      *oauth2.Token
	tokenPath string
	mu        sync.Mutex
	warnOnce  sync.Once
}

// Token returns a cached or refreshed token, persisting it whenever it differs
// from the last one seen. A write failure is logged rather than returned: the
// caller still has a usable token, only reuse across restarts is lost.
func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		if IsInvalidGrant(err) {
			// Once per process: every call fails until re-authentication.
			p.warnOnce.Do(func() {
				logger.Error().Msgf("Google Calendar OAuth grant was revoked or expired; delete %q and re-run the bot interactively to re-authenticate",
					p.tokenPath)
			})
		}

		return nil, err
	}

	// Held across saveToken so concurrent refreshes cannot reorder the rename.
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

// GetClient returns an authenticated client, running the interactive flow if
// tokenPath holds no token, and persisting any rotation.
func GetClient(ctx context.Context, config *oauth2.Config, tokenPath string) (*http.Client, error) {
	tok, err := tokenFromFile(tokenPath)
	saveToFile := false

	//nolint:nestif
	if err == nil {
		if !tok.Valid() {
			src := config.TokenSource(ctx, tok)

			newTok, err := src.Token()
			if err != nil {
				if IsInvalidGrant(err) {
					return nil, fmt.Errorf("%w: delete %q and re-run the bot interactively to re-authenticate",
						ErrOAuthGrantRevoked, tokenPath)
				}

				return nil, err
			}

			// Unpersisted, startup bricks once the old token expires.
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

	// Persist refreshes so restarts survive rotation.
	ts := &persistingTokenSource{
		src:       config.TokenSource(ctx, tok),
		tokenPath: tokenPath,
		last:      tok,
	}

	return oauth2.NewClient(ctx, ts), nil
}

// authCallback carries the consent flow's outcome: an auth code, or the terminal
// cause — state mismatch, denial, or a server that stopped serving.
type authCallback struct {
	err  error
	code string
}

// serveCallback runs the loopback consent server, reporting an unexpected stop
// through tokChan rather than ending the process.
//
// Reporting, not logger.Fatal: os.Exit here would skip getTokenFromWeb's
// deferred Shutdown and leak the listener, so the next run could not bind the
// callback port — exactly what that Shutdown exists to prevent.
//
// once is shared with the callback handlers, so the first outcome wins and the
// channel closes once; tokChan is buffered, so this send never blocks even after
// the caller stops listening. ErrServerClosed is the ordinary stop.
func serveCallback(s *http.Server, listener net.Listener, tokChan chan<- authCallback, once *sync.Once) {
	logger.Debug().Msgf("starting HTTP listener on: %v", s.Addr)

	// Serve on pre-bound listener; ListenAndServe would race by re-binding.
	if err := s.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		once.Do(func() {
			tokChan <- authCallback{err: fmt.Errorf("%w: %w", ErrOAuthHTTPServer, err)}
			close(tokChan)
		})
	}
}

// getTokenFromWeb runs the interactive consent flow: serve a loopback callback,
// open the browser, exchange the returned code. State is CSRF-checked and the
// wait is bounded by AuthTimeout.
func getTokenFromWeb(ctx context.Context, config *oauth2.Config) (*oauth2.Token, error) {
	// CSRF protection for the callback.
	authReqState, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOAuthUUID, err)
	}

	tokChan := make(chan authCallback, 1)

	var once sync.Once

	// Bind first, so authURL carries the actual port. Google requires loopback.
	authListenHost := net.JoinHostPort(AuthListenAddr, strconv.Itoa(AuthListenPort))

	// ListenConfig: a cancelled ctx aborts the bind.
	var lc net.ListenConfig

	listener, err := lc.Listen(ctx, "tcp", authListenHost)
	if err != nil {
		logger.Warn().Msgf("Preferred OAuth callback port %v unavailable (%v), falling back to an ephemeral port",
			AuthListenPort, err)

		listener, err = lc.Listen(ctx, "tcp", net.JoinHostPort(AuthListenAddr, "0"))
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
	// Detached: shutdown needs its grace period even once ctx is cancelled, or
	// the listener leaks and the next run cannot bind the callback port.
	defer func() { //nolint:contextcheck
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

		q := req.URL.Query()

		// Constant-time, to avoid a timing leak on malformed callbacks.
		expectedState := authReqState.String()
		if receivedState := q.Get("state"); subtle.ConstantTimeCompare([]byte(receivedState), []byte(expectedState)) != 1 {
			w.WriteHeader(http.StatusBadRequest)

			if err := t.ExecuteTemplate(w, "failure.html", map[string]any{"error": ErrInvalidCallbackState}); err != nil {
				logger.Error().Msgf("template execution failed: %v", err)
			}

			once.Do(func() {
				tokChan <- authCallback{err: ErrInvalidCallbackState}
				close(tokChan)
			})

			return
		}

		// Google signals denial through the error param; report the real cause
		// rather than render a false success page.
		if cbErr := q.Get("error"); cbErr != "" {
			if err := t.ExecuteTemplate(w, "failure.html", map[string]any{"error": cbErr}); err != nil {
				logger.Error().Msgf("template execution failed: %v", err)
			}

			once.Do(func() {
				tokChan <- authCallback{err: fmt.Errorf("%w: %s", ErrOAuthDenied, cbErr)}
				close(tokChan)
			})

			return
		}

		once.Do(func() {
			tokChan <- authCallback{code: q.Get("code")}
			close(tokChan)
		})

		if err := t.ExecuteTemplate(w, "success.html", map[string]any{}); err != nil {
			logger.Error().Msgf("template execution failed: %v", err)
		}
	})

	r.Get("/favicon.ico", func(w http.ResponseWriter, req *http.Request) {
		http.ServeFileFS(w, req, contentFS, "assets/favicon.ico")
	})

	go serveCallback(&s, listener, tokChan, &once)

	logger.Info().Msgf("Opening local Web server through system browser: %v", authURL)

	if err := browserOpen(authURL); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOAuthBrowser, err)
	}

	var cb authCallback

	authTimer := time.NewTimer(AuthTimeout)
	defer authTimer.Stop()

	select {
	case cb = <-tokChan:
		authTimer.Stop()
	case <-authTimer.C:
		return nil, ErrOAuthTimeout
	case <-ctx.Done():
		// SIGTERM must abort promptly, not wait out AuthTimeout.
		return nil, ctx.Err()
	}

	if cb.err != nil {
		return nil, cb.err
	}

	// Valid state but neither error nor code: malformed.
	if cb.code == "" {
		return nil, ErrInvalidCallbackState
	}

	tok, err := config.Exchange(ctx, cb.code)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOAuthTokenFetch, err)
	}

	return tok, nil
}

// tokenFromFile decodes the token at tokenPath.
func tokenFromFile(tokenPath string) (*oauth2.Token, error) {
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return nil, err
	}

	tok := &oauth2.Token{}

	err = json.NewDecoder(bytes.NewBuffer(b)).Decode(tok)

	return tok, err
}

// ValidateTokenFile reports whether tokenPath decodes. Presence alone is not
// enough: a truncated or hand-edited file would otherwise fail every poll cycle
// at runtime instead of once, clearly, at startup.
func ValidateTokenFile(tokenPath string) error {
	_, err := tokenFromFile(tokenPath)

	return err
}

// IsInvalidGrant reports a revoked or expired refresh token. Only interactive
// re-authentication helps; retrying is pointless.
func IsInvalidGrant(err error) bool {
	var re *oauth2.RetrieveError

	return errors.As(err, &re) && re.ErrorCode == "invalid_grant"
}

// saveToken atomically writes token as JSON at DefaultPerms.
func saveToken(tokenPath string, token *oauth2.Token) error {
	buf := new(bytes.Buffer)

	err := json.NewEncoder(buf).Encode(token) //nolint:gosec // G117: token cache; 0600 below carries confidentiality
	if err != nil {
		return fmt.Errorf("%w: %w", ErrOAuthTokenEncode, err)
	}

	if err = maybe.WriteFile(tokenPath, buf.Bytes(), DefaultPerms); err != nil {
		return fmt.Errorf("%w: %w", ErrOAuthTokenSave, err)
	}

	return nil
}

// LoggingMiddleware logs one line per request.
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
