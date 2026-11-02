// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// A server that accepts a request and never answers must not hold the send:
// the context only ends on shutdown, so it would stall the poll loop for good.
func TestSlackClientBoundsAStalledServer(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer server.Close()
	defer close(release)

	cli := newSlackClient("test-token", 50*time.Millisecond, slack.OptionAPIURL(server.URL+"/"))

	done := make(chan error, 1)

	go func() {
		_, _, err := cli.PostMessageContext(context.Background(), "C123", slack.MsgOptionText("x", false))
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Error("PostMessageContext() = nil, want a timeout")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PostMessageContext() still blocked on a stalled server")
	}
}
