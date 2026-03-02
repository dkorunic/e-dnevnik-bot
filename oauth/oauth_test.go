// @license
// Copyright (C) 2025 Dinko Korunic
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
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package oauth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestTokenFileOperations(t *testing.T) {
	t.Parallel()
	// Create a temporary file for testing.
	tmpfile, err := os.CreateTemp("", "test-token.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())

	// Create a mock token.
	expectedToken := &oauth2.Token{
		AccessToken:  "test-access-token",
		TokenType:    "Bearer",
		RefreshToken: "test-refresh-token",
		Expiry:       time.Now().Add(1 * time.Hour).Round(time.Second),
	}

	// Save the token to the file.
	if err := saveToken(tmpfile.Name(), expectedToken); err != nil {
		t.Fatalf("saveToken failed: %v", err)
	}

	// Read the token from the file.
	actualToken, err := tokenFromFile(tmpfile.Name())
	if err != nil {
		t.Fatalf("tokenFromFile failed: %v", err)
	}

	// Compare the tokens.
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
	// Saving to a path inside a non-existent directory should fail.
	err := saveToken("/non/existent/dir/token.json", &oauth2.Token{})
	if err == nil {
		t.Error("saveToken() should fail when destination directory does not exist")
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
