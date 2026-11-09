// SPDX-FileCopyrightText: 2023 Dinko Korunic
// SPDX-License-Identifier: MIT

package oauth

import (
	"bytes"
	"cmp"
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
	"slices"
	"strconv"
	"strings"
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
	ErrOAuthTokenMissing    = errors.New("no usable OAuth token file")
)

// httpTimeout bounds every Google request, refreshes included: oauth2 defaults to
// http.DefaultClient and the context only ends on shutdown, so a server that never
// answers would stall the poll loop for good.
var httpTimeout = 30 * time.Second

// browserOpen is a test seam; production uses browser.OpenURL.
var browserOpen = browser.OpenURL

//go:embed templates/*html assets/*ico
var contentFS embed.FS

// persistingTokenSource writes refreshed tokens to disk, so a restart survives
// rotation.
type persistingTokenSource struct {
	src        oauth2.TokenSource
	last       *oauth2.Token
	tokenPath  string
	clientID   string
	mu         sync.Mutex
	grantOnce  sync.Once // invalid_grant explained once
	clientOnce sync.Once // client rejection explained once; separate, so neither hides the other
}

// Token returns a cached or refreshed token, persisting it whenever it differs
// from the last one saved. A write failure is logged rather than returned: the
// caller still has a usable token, only reuse across restarts is lost.
func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		// Once per process: every call fails until the cause is fixed.
		switch {
		case IsInvalidGrant(err):
			p.grantOnce.Do(func() {
				logger.Error().Msgf("Google Calendar OAuth grant was revoked or expired; delete %q and re-run the bot interactively to re-authenticate",
					p.tokenPath)
			})
		case IsAuthRejected(err):
			// Two causes share these codes: a saved token issued to another
			// client ID (after an upgrade that ships new credentials —
			// re-authenticating fixes it), or this build's compiled-in
			// client being deleted or disabled (only a newer release does).
			p.clientOnce.Do(func() {
				logger.Error().Msgf("Google Calendar rejected the OAuth client (%v); exams are queued. Delete %q and re-run the bot interactively to re-authenticate; if that fails too, update e-dnevnik-bot to a release with working credentials",
					err, p.tokenPath)
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
		// Only once saved, so a failed write is retried on the next call.
		if serr := saveToken(p.tokenPath, tok, p.clientID); serr != nil {
			logger.Warn().Msgf("Unable to persist refreshed OAuth token: %v", serr)
		} else {
			p.last = tok
		}
	}

	return tok, nil
}

// GetClient returns an authenticated client, persisting any rotation. A
// tokenPath holding no token runs the interactive consent flow only when
// interactive is set; otherwise it fails with ErrOAuthTokenMissing at once.
// Poll cycles pass false: the flow binds the callback port and waits out
// AuthTimeout for a consent nobody watching a daemon will give.
func GetClient(ctx context.Context, config *oauth2.Config, tokenPath string, interactive bool) (*http.Client, error) {
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Timeout: httpTimeout})

	tok, err := tokenFromFile(tokenPath)
	saved := true

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

				// Also what a token issued to another client ID gets, after an
				// upgrade that ships a new one.
				if IsAuthRejected(err) {
					return nil, fmt.Errorf("%w: delete %q and re-run the bot interactively to re-authenticate", err, tokenPath)
				}

				return nil, err
			}

			// Unpersisted, startup bricks once the old token expires. Only warned
			// about, as in persistingTokenSource: this one is usable regardless.
			if newTok.AccessToken != tok.AccessToken || newTok.RefreshToken != tok.RefreshToken {
				tok = newTok

				if serr := saveToken(tokenPath, tok, config.ClientID); serr != nil {
					logger.Warn().Msgf("Unable to persist refreshed OAuth token: %v", serr)

					saved = false
				}
			}
		}
	} else {
		if !interactive {
			return nil, fmt.Errorf("%w: %q (%w); re-run the bot interactively to authenticate",
				ErrOAuthTokenMissing, tokenPath, err)
		}

		tok, err = getTokenFromWeb(ctx, config)
		if err != nil {
			return nil, err
		}

		// Unlike a refresh, a consent lost here has to be given again.
		if err = saveToken(tokenPath, tok, config.ClientID); err != nil {
			return nil, err
		}
	}

	// Persist refreshes so restarts survive rotation.
	ts := &persistingTokenSource{
		src:       config.TokenSource(ctx, tok),
		tokenPath: tokenPath,
		clientID:  config.ClientID,
		last:      tok,
	}

	// Unseen, so the next use saves it: it may be the only copy of a rotated
	// refresh token.
	if !saved {
		ts.last = nil
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

	// Bind first, so authURL carries the actual port. Google requires loopback,
	// and accepts any port of it for a desktop client, which is what makes the
	// fallback below work.
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

	// PKCE: the client secret ships in every binary, so it cannot prove who redeems
	// the code.
	verifier := oauth2.GenerateVerifier()

	authCodeURL := config.AuthCodeURL(authReqState.String(), oauth2.AccessTypeOffline, oauth2.ApprovalForce,
		oauth2.S256ChallengeOption(verifier))

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

	// Logged here, not in serveCallback: getTokenFromWeb does not wait for that
	// goroutine, so its logging could outlive the call.
	logger.Debug().Msgf("starting HTTP listener on: %v", s.Addr)

	go serveCallback(&s, listener, tokChan, &once)

	logger.Info().Msgf("Opening local Web server through system browser: %v", authURL)

	hint := callbackAccessHint(listener.Addr().String(), authURL)

	// Over SSH, a browser that opens may be on the far end's screen (macOS open
	// exits 0 on the console session), so its success proves nothing.
	overSSH := os.Getenv("SSH_CONNECTION") != ""
	if overSSH {
		logger.Warn().Msgf("Connected over SSH: %v", hint)
	}

	// Not awaited: browser.OpenURL waits for the command to exit, and a terminal
	// browser stays in the foreground, which would hold off both AuthTimeout and
	// SIGTERM below. Not fatal: the hint is the way in.
	openBrowser := browserOpen

	go func() {
		err := openBrowser(authURL)

		switch {
		case err == nil:
		case overSSH:
			logger.Warn().Msgf("%v (%v)", ErrOAuthBrowser, err)
		default:
			logger.Warn().Msgf("%v (%v); %v", ErrOAuthBrowser, err, hint)
		}
	}()

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

	tok, err := config.Exchange(ctx, cb.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOAuthTokenFetch, err)
	}

	return tok, nil
}

// sshTunnelCommand returns the command that forwards addr, the callback's listen
// address, from the user's own machine. A session's SSH_CONNECTION names this host
// as the user reached it; sudo usually strips it, leaving the hostname. Best effort
// behind NAT or a jump host, where the user knows the address and this host does not.
func sshTunnelCommand(addr string) string {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		port = strconv.Itoa(AuthListenPort)
	}

	// Nothing visible from inside names the host the user reached.
	if inContainer() {
		return "ssh -L " + port + ":" + addr + " <docker-host>"
	}

	host, _ := os.Hostname()
	opts := ""

	// Format: client address, client port, server address, server port.
	if f := strings.Fields(os.Getenv("SSH_CONNECTION")); len(f) == 4 {
		host = f[2]

		if f[3] != "22" {
			opts = "-p " + f[3] + " "
		}
	}

	host = cmp.Or(host, "<server>")

	if u := cmp.Or(os.Getenv("SUDO_USER"), os.Getenv("USER")); u != "" {
		host = u + "@" + host
	}

	return "ssh " + opts + "-L " + port + ":" + addr + " " + host
}

// containerMarkers are files a container runtime leaves in the root filesystem.
var containerMarkers = []string{"/.dockerenv", "/run/.containerenv"}

// inContainer reports whether this process runs in a container.
func inContainer() bool {
	for _, m := range containerMarkers {
		if _, err := os.Stat(m); err == nil {
			return true
		}
	}

	return false
}

// callbackAccessHint tells the user how to reach the callback from elsewhere. The
// callback listens on loopback, which in a container is not the host's, and which
// publishing a port cannot reach either.
func callbackAccessHint(addr, authURL string) string {
	hint := fmt.Sprintf("on a machine with a browser, run %q, then open %v within %v", sshTunnelCommand(addr), authURL, AuthTimeout)
	if inContainer() {
		return "the callback cannot be reached from outside this container unless it already uses --network host; " + hint +
			"; with no SSH access, authorize on a desktop and copy the token file here (see the README)"
	}

	return hint
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

// TokenClientID returns the client that tokenPath's token belongs to, or "" for a
// token saved before clients were recorded. An error means no usable token.
func TokenClientID(tokenPath string) (string, error) {
	b, err := os.ReadFile(tokenPath)
	if err != nil {
		return "", err
	}

	var stored struct {
		ClientID string `json:"client_id"` //nolint:tagliatelle // see storedToken
	}

	if err := json.NewDecoder(bytes.NewBuffer(b)).Decode(&stored); err != nil {
		return "", err
	}

	return stored.ClientID, nil
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

// authRejectedCodes are the token endpoint's RFC 6749 errors that no retry can
// cure: a revoked or expired grant, or an OAuth client deleted, disabled or
// with a rotated secret.
var authRejectedCodes = []string{"invalid_grant", "invalid_client", "unauthorized_client"}

// IsAuthRejected reports a token-endpoint rejection only re-authentication or a
// new client fixes: IsInvalidGrant, plus a deleted or reconfigured client.
func IsAuthRejected(err error) bool {
	var re *oauth2.RetrieveError

	return errors.As(err, &re) && slices.Contains(authRejectedCodes, re.ErrorCode)
}

// storedToken is the token file: a refresh token works only with the client that
// issued it, so the file names that client beside the token's own fields.
type storedToken struct {
	*oauth2.Token

	ClientID string `json:"client_id,omitempty"` //nolint:tagliatelle // beside oauth2.Token's snake_case fields
}

// saveToken atomically writes token and its client as JSON at DefaultPerms.
func saveToken(tokenPath string, token *oauth2.Token, clientID string) error {
	buf := new(bytes.Buffer)

	err := json.NewEncoder(buf).Encode(storedToken{Token: token, ClientID: clientID})
	if err != nil {
		return fmt.Errorf("%w: %w", ErrOAuthTokenEncode, err)
	}

	if err = maybe.WriteFile(tokenPath, buf.Bytes(), DefaultPerms); err != nil {
		return fmt.Errorf("%w: %w", ErrOAuthTokenSave, err)
	}

	return nil
}

// LoggingMiddleware logs one line per request. The path, not the URI: the
// callback's query carries the auth code and state.
func LoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startTime := time.Now()

		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)

		next.ServeHTTP(ww, r)

		reqDuration := time.Since(startTime)

		clientIP, _, _ := net.SplitHostPort(r.RemoteAddr)

		logger.Debug().Msgf("OAuth HTTP server request: method: %v, path: %v, status: %v, client ip: %v, duration: %v",
			r.Method, r.URL.Path, ww.Status(), clientIP, reqDuration)
	})
}
