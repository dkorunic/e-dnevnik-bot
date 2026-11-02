// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func embeddedClient(t *testing.T, path, kind string) string {
	t.Helper()

	b, err := credentialFS.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q) = %v", path, err)
	}

	var creds map[string]struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(b, &creds); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}

	c, ok := creds[kind]
	if !ok || c.ClientID == "" {
		t.Fatalf("%v holds no %q client", path, kind)
	}

	return c.ClientID
}

// A refresh token works only with the client that issued it, so every existing
// install has to keep the web client, and only a new setup gets the desktop one.
func TestCalendarOAuthConfigPicksTheTokensClient(t *testing.T) {
	t.Parallel()

	desktop := embeddedClient(t, CalendarCredentials, "installed")
	legacy := embeddedClient(t, CalendarLegacyCredentials, "web")

	if desktop == legacy {
		t.Fatal("both embedded credentials are the same client")
	}

	tokenWith := func(marker string) string {
		field := ""
		if marker != "" {
			field = fmt.Sprintf(`,"client_id":%q`, marker)
		}

		return fmt.Sprintf(`{"access_token":"a","token_type":"Bearer","refresh_token":"r","expiry":"2099-01-01T00:00:00Z"%s}`, field)
	}

	tests := []struct {
		name string
		file string // no file at all when empty
		want string
	}{
		{name: "new setup, no token", want: desktop},
		{name: "existing install, no marker", file: tokenWith(""), want: legacy},
		{name: "existing install after a refresh", file: tokenWith(legacy), want: legacy},
		{name: "new setup after consent", file: tokenWith(desktop), want: desktop},
		{name: "a client this build does not know", file: tokenWith("other"), want: desktop},
		{name: "unreadable token, which goes to consent", file: "garbage {", want: desktop},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "token.json")

			if tt.file != "" {
				if err := os.WriteFile(path, []byte(tt.file), 0o600); err != nil {
					t.Fatalf("WriteFile() = %v", err)
				}
			}

			cfg, err := calendarOAuthConfig(path)
			if err != nil {
				t.Fatalf("calendarOAuthConfig() = %v", err)
			}

			if cfg.ClientID != tt.want {
				t.Errorf("client = %q, want %q", cfg.ClientID, tt.want)
			}

			if len(cfg.Scopes) == 0 {
				t.Error("config has no scopes")
			}
		})
	}
}
