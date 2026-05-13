// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

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
