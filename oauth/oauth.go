// @license
// Copyright (C) 2022  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package oauth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/gofrs/uuid"
	jsoniter "github.com/json-iterator/go"
	"github.com/phayes/freeport"
	"github.com/pkg/browser"
	"golang.org/x/oauth2"
)

const (
	AuthTimeout    = 90 * time.Second
	AuthListenAddr = "127.0.0.1"
	AuthScheme     = "http://"
)

var (
	ErrOAuthUUID        = errors.New("unable to generate UUID")
	ErrOAuthFreePort    = errors.New("unable to get a free port")
	ErrOAuthHTTPServer  = errors.New("unable to start HTTP server")
	ErrOAuthBrowser     = errors.New("unable to open system browser")
	ErrOAuthTimeout     = errors.New("timeout while waiting for authentication to finish")
	ErrOAuthTokenFetch  = errors.New("unable to retrieve token from Google API")
	ErrOAuthTokenSave   = errors.New("unable to save token to file")
	ErrOAuthTokenEncode = errors.New("unable to encode OAuth token to JSON")
)

// GetClient retrieves an HTTP client with OAuth2 authentication.
//
// It takes the following parameters:
// - ctx: the context.Context to use for the HTTP client.
// - config: the *oauth2.Config object containing the OAuth2 configuration.
// - tokenPath: the path to the token file.
//
// It returns a *http.Client and an error.
func GetClient(ctx context.Context, config *oauth2.Config, tokenPath string) (*http.Client, error) {
	tok, err := tokenFromFile(tokenPath)
	if err != nil {
		tok, err = getTokenFromWeb(ctx, config)
		if err != nil {
			return nil, err
		}

		if err = saveToken(tokenPath, tok); err != nil {
			return nil, err
		}
	}

	return config.Client(ctx, tok), nil
}

// getTokenFromWeb retrieves an OAuth2 token from a web-based authentication flow.
//
// ctx is the context.Context to use for the request.
// config is the *oauth2.Config object that contains the OAuth2 configuration.
// It returns the retrieved *oauth2.Token and an error if any occurred.
func getTokenFromWeb(ctx context.Context, config *oauth2.Config) (*oauth2.Token, error) {
	// random UUID as a state
	authReqState, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOAuthUUID, err)
	}

	tokChan := make(chan string, 1)

	// get a free random port
	authListenPort, err := freeport.GetFreePort()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOAuthFreePort, err)
	}

	// redirect uri listener for auth callback
	authListenHost := net.JoinHostPort(AuthListenAddr, strconv.Itoa(authListenPort))

	// oauth config auth redirect uri
	config.RedirectURL = AuthScheme + authListenHost

	s := http.Server{
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       60 * time.Second,
		ReadHeaderTimeout: 10 * time.Second,
		Addr:              authListenHost,
	}
	defer s.Close()

	// oauth callback handler
	s.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if actualState := r.URL.Query().Get("state"); actualState != authReqState.String() {
			http.Error(w, "Invalid authentication state", http.StatusUnauthorized)

			return
		}

		tokChan <- r.URL.Query().Get("code")
		close(tokChan)
		_, _ = w.Write([]byte("Authentication complete, you can close this window."))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	// oauth callback server
	go func() {
		if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Fatal().Msgf("%v: %v", ErrOAuthHTTPServer, err)
		}
	}()

	authCodeURL := config.AuthCodeURL(authReqState.String(), oauth2.AccessTypeOffline)
	logger.Info().Msgf("Opening auth URL through system browser: %v", authCodeURL)

	// oauth dialog through system browser
	if err := browser.OpenURL(authCodeURL); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOAuthBrowser, err)
	}

	var authCode string

	ticker := time.NewTimer(AuthTimeout)
	defer ticker.Stop()

	select {
	case authCode = <-tokChan:
		ticker.Stop()

		break
	case <-ticker.C:
		return nil, ErrOAuthTimeout
	}

	tok, err := config.Exchange(ctx, authCode)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrOAuthTokenFetch, err)
	}

	return tok, nil
}

// tokenFromFile retrieves an OAuth2 token from a file.
//
// It takes a string parameter `tokenPath` which represents the path to the token file.
// It returns a pointer to `oauth2.Token` and an error.
func tokenFromFile(tokenPath string) (*oauth2.Token, error) {
	f, err := os.Open(tokenPath)
	if err != nil {
		return nil, err
	}

	defer func() {
		cerr := f.Close()
		if err == nil {
			err = cerr
		}
	}()

	tok := &oauth2.Token{}
	json := jsoniter.ConfigCompatibleWithStandardLibrary
	err = json.NewDecoder(f).Decode(tok)

	return tok, err
}

// saveToken saves the OAuth2 token to the specified path.
//
// It takes the token path as a string and the token as a pointer to oauth2.Token.
// It returns an error if there was an issue saving the token.
func saveToken(tokenPath string, token *oauth2.Token) (err error) {
	var f *os.File

	f, err = os.OpenFile(tokenPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrOAuthTokenSave, err)
	}

	defer func() {
		cerr := f.Close()
		if err == nil && cerr != nil {
			err = fmt.Errorf("%w: %w", ErrOAuthTokenSave, cerr)
		}
	}()

	json := jsoniter.ConfigCompatibleWithStandardLibrary

	err = json.NewEncoder(f).Encode(token)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrOAuthTokenEncode, err)
	}

	return nil
}
