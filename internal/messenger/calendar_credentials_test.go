// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"encoding/json"
	"testing"

	"golang.org/x/oauth2/google"
	"google.golang.org/api/calendar/v3"
)

// Credentials compiled into a public binary have to be a desktop client: Google
// treats its secret as not confidential and accepts any loopback port. A web
// client needs an exact redirect, which the callback port fallback cannot meet.
func TestEmbeddedCalendarCredentialsAreADesktopClient(t *testing.T) {
	t.Parallel()

	b, err := credentialFS.ReadFile(CalendarCredentials)
	if err != nil {
		t.Fatalf("ReadFile() = %v", err)
	}

	var creds map[string]json.RawMessage
	if err := json.Unmarshal(b, &creds); err != nil {
		t.Fatalf("Unmarshal() = %v", err)
	}

	if _, ok := creds["installed"]; !ok {
		t.Error("embedded credentials are not an \"installed\" (desktop) client")
	}

	cfg, err := google.ConfigFromJSON(b, calendar.CalendarEventsScope)
	if err != nil {
		t.Fatalf("ConfigFromJSON() = %v", err)
	}

	if cfg.ClientID == "" || cfg.Endpoint.TokenURL == "" {
		t.Errorf("incomplete config: client ID set = %v, token URL = %q", cfg.ClientID != "", cfg.Endpoint.TokenURL)
	}
}
