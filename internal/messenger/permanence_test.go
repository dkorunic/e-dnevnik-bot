// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/avast/retry-go/v5"
	"github.com/bwmarrin/discordgo"
	"github.com/go-telegram/bot"
	"github.com/slack-go/slack"
	"github.com/wneessen/go-mail"
	"go.mau.fi/whatsmeow"
	"google.golang.org/api/googleapi"
)

// TestIsPermanentHTTPStatus pins the status-code split that four of the six
// messengers share. 408 and 429 are the two client errors that are worth
// retrying; classifying either as permanent poison-drops an alert that would
// have gone through on the next attempt.
func TestIsPermanentHTTPStatus(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code int
		want bool
	}{
		{name: "400 bad request is permanent", code: http.StatusBadRequest, want: true},
		{name: "401 unauthorized is permanent", code: http.StatusUnauthorized, want: true},
		{name: "403 forbidden is permanent", code: http.StatusForbidden, want: true},
		{name: "404 not found is permanent", code: http.StatusNotFound, want: true},
		{name: "410 gone is permanent", code: http.StatusGone, want: true},
		{name: "499 is permanent", code: 499, want: true},
		{name: "408 request timeout is retriable", code: http.StatusRequestTimeout, want: false},
		{name: "429 too many requests is retriable", code: http.StatusTooManyRequests, want: false},
		{name: "500 is retriable", code: http.StatusInternalServerError, want: false},
		{name: "502 is retriable", code: http.StatusBadGateway, want: false},
		{name: "503 is retriable", code: http.StatusServiceUnavailable, want: false},
		{name: "200 is not an error at all", code: http.StatusOK, want: false},
		{name: "399 is below the client-error range", code: 399, want: false},
		{name: "zero value is not permanent", code: 0, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := isPermanentHTTPStatus(tt.code); got != tt.want {
				t.Errorf("isPermanentHTTPStatus(%d) = %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

// markFuncs pairs each messenger's classifier with its name for table reuse.
var markFuncs = map[string]func(error) error{
	"discord":  markDiscordPermanent,
	"telegram": markTelegramPermanent,
	"slack":    markSlackPermanent,
	"mail":     markMailPermanent,
	"whatsapp": markWhatsAppPermanent,
	"calendar": markCalendarPermanent,
}

// TestMarkPermanentNilIsNil: every classifier must pass nil through untouched,
// since the send paths call it unconditionally on the result of a send.
func TestMarkPermanentNilIsNil(t *testing.T) {
	t.Parallel()

	for name, mark := range markFuncs {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := mark(nil); got != nil {
				t.Errorf("mark(nil) = %v, want nil", got)
			}
		})
	}
}

// TestMarkPermanentClassification is the full permanent/transient matrix. The
// transient half is the one that matters most: a permanent classification means
// the recipient is poison-dropped and the message is never retried.
func TestMarkPermanentClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mark      func(error) error
		err       error
		permanent bool
	}{
		// Discord: classification rides on the REST response status.
		{
			name:      "discord 401 is permanent",
			mark:      markDiscordPermanent,
			err:       &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusUnauthorized}},
			permanent: true,
		},
		{
			name:      "discord 404 is permanent",
			mark:      markDiscordPermanent,
			err:       &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusNotFound}},
			permanent: true,
		},
		{
			name:      "discord 429 is transient",
			mark:      markDiscordPermanent,
			err:       &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusTooManyRequests}},
			permanent: false,
		},
		{
			name:      "discord 500 is transient",
			mark:      markDiscordPermanent,
			err:       &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusInternalServerError}},
			permanent: false,
		},
		{
			name:      "discord REST error without a response is transient",
			mark:      markDiscordPermanent,
			err:       &discordgo.RESTError{},
			permanent: false,
		},

		// Telegram: rate limiting must never be permanent, since it is the
		// single most common failure on a busy chat.
		{
			name:      "telegram too-many-requests error is transient",
			mark:      markTelegramPermanent,
			err:       &bot.TooManyRequestsError{Message: "retry later", RetryAfter: 30},
			permanent: false,
		},
		{
			name:      "telegram ErrorTooManyRequests is transient",
			mark:      markTelegramPermanent,
			err:       bot.ErrorTooManyRequests,
			permanent: false,
		},
		{
			name:      "telegram unauthorized is permanent",
			mark:      markTelegramPermanent,
			err:       bot.ErrorUnauthorized,
			permanent: true,
		},
		{
			name:      "telegram bad request is permanent",
			mark:      markTelegramPermanent,
			err:       bot.ErrorBadRequest,
			permanent: true,
		},
		{
			name:      "telegram not found is permanent",
			mark:      markTelegramPermanent,
			err:       bot.ErrorNotFound,
			permanent: true,
		},
		{
			name:      "telegram conflict is permanent",
			mark:      markTelegramPermanent,
			err:       bot.ErrorConflict,
			permanent: true,
		},
		{
			name:      "telegram group migration is permanent",
			mark:      markTelegramPermanent,
			err:       &bot.MigrateError{Message: "group upgraded", MigrateToChatID: -100123},
			permanent: true,
		},

		// Slack.
		{
			name:      "slack 429 is transient",
			mark:      markSlackPermanent,
			err:       slack.StatusCodeError{Code: http.StatusTooManyRequests},
			permanent: false,
		},
		{
			name:      "slack 503 is transient",
			mark:      markSlackPermanent,
			err:       slack.StatusCodeError{Code: http.StatusServiceUnavailable},
			permanent: false,
		},
		{
			name:      "slack 404 is permanent",
			mark:      markSlackPermanent,
			err:       slack.StatusCodeError{Code: http.StatusNotFound},
			permanent: true,
		},
		{
			name:      "slack API error response is permanent",
			mark:      markSlackPermanent,
			err:       &slack.SlackErrorResponse{Err: "channel_not_found"},
			permanent: true,
		},

		// Mail: SMTP errors carry their own temporary/permanent flag.
		{
			name:      "mail permanent SMTP failure is permanent",
			mark:      markMailPermanent,
			err:       &mail.SendError{Reason: mail.ErrSMTPMailFrom},
			permanent: true,
		},
		{
			name:      "mail non-SMTP error is transient",
			mark:      markMailPermanent,
			err:       errors.New("dial tcp: connection refused"),
			permanent: false,
		},

		// WhatsApp: each sentinel means the session or recipient is unusable.
		{
			name:      "whatsapp nil client is permanent",
			mark:      markWhatsAppPermanent,
			err:       whatsmeow.ErrClientIsNil,
			permanent: true,
		},
		{
			name:      "whatsapp not logged in is permanent",
			mark:      markWhatsAppPermanent,
			err:       whatsmeow.ErrNotLoggedIn,
			permanent: true,
		},
		{
			name:      "whatsapp broadcast list is permanent",
			mark:      markWhatsAppPermanent,
			err:       whatsmeow.ErrBroadcastListUnsupported,
			permanent: true,
		},
		{
			name:      "whatsapp unknown server is permanent",
			mark:      markWhatsAppPermanent,
			err:       whatsmeow.ErrUnknownServer,
			permanent: true,
		},
		{
			name:      "whatsapp network failure is transient",
			mark:      markWhatsAppPermanent,
			err:       errors.New("websocket: close 1006"),
			permanent: false,
		},

		// Calendar.
		{
			name:      "calendar 403 is permanent",
			mark:      markCalendarPermanent,
			err:       &googleapi.Error{Code: http.StatusForbidden},
			permanent: true,
		},
		{
			name:      "calendar 429 is transient",
			mark:      markCalendarPermanent,
			err:       &googleapi.Error{Code: http.StatusTooManyRequests},
			permanent: false,
		},
		{
			name:      "calendar 500 is transient",
			mark:      markCalendarPermanent,
			err:       &googleapi.Error{Code: http.StatusInternalServerError},
			permanent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := tt.mark(tt.err)

			if isPermanentSendErr(got) != tt.permanent {
				t.Fatalf("mark(%v) classified permanent=%v, want %v", tt.err, !tt.permanent, tt.permanent)
			}

			// The original error must stay inspectable, or operators lose the
			// only diagnostic they get for a poison-dropped recipient.
			if !errors.Is(got, tt.err) {
				t.Errorf("mark(%v) = %v, which no longer unwraps to the original error", tt.err, got)
			}
		})
	}
}

// TestMarkPermanentSurvivesWrapping: send paths wrap library errors with
// context before classifying. errors.As/Is must see through that, or every
// wrapped permanent failure silently degrades into an endless retry.
func TestMarkPermanentSurvivesWrapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mark func(error) error
		err  error
	}{
		{
			name: "discord",
			mark: markDiscordPermanent,
			err:  &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusForbidden}},
		},
		{name: "telegram", mark: markTelegramPermanent, err: bot.ErrorForbidden},
		{name: "slack", mark: markSlackPermanent, err: slack.StatusCodeError{Code: http.StatusForbidden}},
		{name: "mail", mark: markMailPermanent, err: &mail.SendError{Reason: mail.ErrSMTPMailFrom}},
		{name: "whatsapp", mark: markWhatsAppPermanent, err: whatsmeow.ErrNotLoggedIn},
		{name: "calendar", mark: markCalendarPermanent, err: &googleapi.Error{Code: http.StatusForbidden}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			wrapped := fmt.Errorf("sending to recipient %q: %w", "123", tt.err)

			if got := tt.mark(wrapped); !isPermanentSendErr(got) {
				t.Errorf("mark(wrapped %v) = %v, want permanent — classification must see through wrapping", tt.err, got)
			}
		})
	}
}

// TestPermanenceSurvivesRetryDo is the subtle one. retry-go v5 strips the outer
// retry.Unrecoverable marker from the error it returns, so permanence is
// carried by an inner permanentError sentinel instead. This checks the property
// end to end, after a real retry loop, for every messenger — and that the loop
// actually short-circuits rather than burning all its attempts.
func TestPermanenceSurvivesRetryDo(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mark func(error) error
		err  error
	}{
		{
			name: "discord",
			mark: markDiscordPermanent,
			err:  &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusForbidden}},
		},
		{name: "telegram", mark: markTelegramPermanent, err: bot.ErrorForbidden},
		{name: "slack", mark: markSlackPermanent, err: slack.StatusCodeError{Code: http.StatusForbidden}},
		{name: "mail", mark: markMailPermanent, err: &mail.SendError{Reason: mail.ErrSMTPMailFrom}},
		{name: "whatsapp", mark: markWhatsAppPermanent, err: whatsmeow.ErrNotLoggedIn},
		{name: "calendar", mark: markCalendarPermanent, err: &googleapi.Error{Code: http.StatusForbidden}},
	}

	const attempts = 5

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			calls := 0

			err := retry.New(
				retry.Attempts(attempts),
				retry.Delay(0),
				retry.MaxDelay(0),
				retry.Context(context.Background()),
			).Do(func() error {
				calls++

				return tt.mark(tt.err)
			})

			if calls != 1 {
				t.Errorf("permanent error was retried %d times, want 1 — retry.Unrecoverable must short-circuit the loop", calls)
			}

			if !isPermanentSendErr(err) {
				t.Errorf("after retry.Do the error is no longer classified permanent (%v); the inner permanentError sentinel is what survives marker stripping", err)
			}
		})
	}
}

// TestTransientErrorsExhaustRetries is the complement: a transient failure must
// use every attempt, and must not come back looking permanent — otherwise it
// would be poison-dropped instead of requeued for the next cycle.
func TestTransientErrorsExhaustRetries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		mark func(error) error
		err  error
	}{
		{
			name: "discord 429",
			mark: markDiscordPermanent,
			err:  &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusTooManyRequests}},
		},
		{name: "telegram rate limit", mark: markTelegramPermanent, err: bot.ErrorTooManyRequests},
		{name: "slack 503", mark: markSlackPermanent, err: slack.StatusCodeError{Code: http.StatusServiceUnavailable}},
		{name: "mail dial failure", mark: markMailPermanent, err: errors.New("connection refused")},
		{name: "whatsapp socket close", mark: markWhatsAppPermanent, err: errors.New("websocket: close 1006")},
		{name: "calendar 500", mark: markCalendarPermanent, err: &googleapi.Error{Code: http.StatusInternalServerError}},
	}

	const attempts = 3

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			calls := 0

			err := retry.New(
				retry.Attempts(attempts),
				retry.Delay(0),
				retry.MaxDelay(0),
				retry.Context(context.Background()),
			).Do(func() error {
				calls++

				return tt.mark(tt.err)
			})

			if calls != attempts {
				t.Errorf("transient error made %d attempts, want %d", calls, attempts)
			}

			if isPermanentSendErr(err) {
				t.Errorf("transient error came back classified permanent (%v); it would be poison-dropped instead of requeued", err)
			}
		})
	}
}
