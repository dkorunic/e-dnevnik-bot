// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/rs/zerolog"
	"github.com/slack-go/slack"
	"go.mau.fi/whatsmeow"
	"go.uber.org/ratelimit"
)

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func TestIsUnreachable(t *testing.T) {
	t.Parallel()

	timeout := &url.Error{Op: "Post", URL: "u", Err: timeoutErr{}}
	reset := &url.Error{Op: "Post", URL: "u", Err: &net.OpError{Op: "read", Err: errors.New("reset")}}

	tests := []struct {
		err  error
		name string
		want bool
	}{
		{name: "nil"},
		{name: "http client timeout", err: &url.Error{Op: "Post", URL: "u", Err: timeoutErr{}}, want: true},
		{name: "wrapped timeout", err: fmt.Errorf("send: %w", &url.Error{Op: "Post", URL: "u", Err: timeoutErr{}}), want: true},
		{name: "refused dial", err: &url.Error{Op: "Post", URL: "u", Err: &net.OpError{Op: "dial", Err: errors.New("refused")}}, want: true},
		{name: "whatsapp send timeout", err: fmt.Errorf("send: %w", whatsmeow.ErrMessageTimedOut), want: true},
		{name: "whatsapp query timeout", err: whatsmeow.ErrIQTimedOut, want: true},
		{name: "reset mid-request", err: &url.Error{Op: "Post", URL: "u", Err: &net.OpError{Op: "read", Err: errors.New("reset")}}},
		{name: "shutdown", err: &url.Error{Op: "Post", URL: "u", Err: context.Canceled}},
		{name: "an API answer", err: errors.New("internal_error")},
		// The state the next message would meet is the last attempt's. errors.As
		// on the aggregate answered for whichever attempt came first.
		{name: "recovered from a timeout, then reset", err: retry.Error{timeout, reset, reset}},
		{name: "reset, then timing out", err: retry.Error{reset, timeout, timeout}, want: true},
		{name: "an API answer after a timeout", err: retry.Error{timeout, errors.New("internal_error")}},
		{name: "shutdown after a timeout", err: retry.Error{timeout, context.Canceled}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isUnreachable(tt.err); got != tt.want {
				t.Errorf("isUnreachable(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// Every recipient of every message would wait out the same timeouts, with all
// other messengers' next poll waiting too: one is enough to stop the cycle.
// Not parallel: swaps the slackCli global.
func TestProcessSlackStalledServerStopsTheCycle(t *testing.T) {
	release := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer server.Close()
	defer close(release)

	orig := slackCli
	slackCli = newSlackClient("t", 50*time.Millisecond, slack.OptionAPIURL(server.URL+"/"))

	t.Cleanup(func() { slackCli = orig })

	msg := msgtypes.Message{Username: "u", Subject: "s", Descriptions: []string{"D"}, Fields: []string{"A"}}

	if !processSlack(t.Context(), newRunTestDB(t), msg, []string{"C1", "C2"}, ratelimit.New(1000), 1) {
		t.Error("processSlack() = false, want the cycle stopped on a server that never answers")
	}
}

// Not parallel: swaps the global logger.
func TestExamEventOfLogOmitsContent(t *testing.T) {
	var buf bytes.Buffer

	orig := logger.Logger
	logger.Logger = zerolog.New(&buf)

	t.Cleanup(func() { logger.Logger = orig })

	examEventOf("test", msgtypes.Message{
		Code:      msgtypes.Exam,
		Timestamp: time.Now().AddDate(0, -1, 0),
		Fields:    []string{"SECRETNOTE"},
	})

	if out := buf.String(); strings.Contains(out, "SECRETNOTE") {
		t.Errorf("log carries event content: %s", out)
	}
}
