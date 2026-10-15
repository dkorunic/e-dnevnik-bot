// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"reflect"
	"testing"
	"time"

	"github.com/avast/retry-go/v5"
	"github.com/bwmarrin/discordgo"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/go-telegram/bot"
	"github.com/slack-go/slack"
	"go.mau.fi/whatsmeow"
	"go.uber.org/ratelimit"
	"google.golang.org/api/googleapi"
)

func TestMergeSkipRecipients(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		existing []string
		extras   []string
		want     []string
	}{
		{
			// Non-empty extras triggers the merge path, which also dedupes existing.
			name:     "duplicates within existing (merge path)",
			existing: []string{"a", "a", "b"},
			extras:   []string{"c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "duplicates within extras",
			existing: []string{"a"},
			extras:   []string{"b", "b", "c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "overlap between slices",
			existing: []string{"a", "b"},
			extras:   []string{"b", "c"},
			want:     []string{"a", "b", "c"},
		},
		{
			// Empty extras short-circuits; callers dedupe before storing.
			name:     "empty extras short-circuits unchanged",
			existing: []string{"a", "b"},
			extras:   nil,
			want:     []string{"a", "b"},
		},
		{
			name:     "both empty",
			existing: nil,
			extras:   nil,
			want:     nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mergeSkipRecipients(tc.existing, tc.extras)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("mergeSkipRecipients(%v, %v) = %v, want %v",
					tc.existing, tc.extras, got, tc.want)
			}
		})
	}
}

func TestTruncateWithEllipsis(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		max   int
		want  string
	}{
		{"shorter than max", "hello", 10, "hello"},
		{"exact max length", "hello", 5, "hello"},
		{"longer than max", "hello world", 8, "hello..."},
		{"empty string", "", 5, ""},
		{"unicode multibyte", "héllo wörld", 8, "héllo..."},
		{"exactly max+1", "abcdef", 5, "ab..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateWithEllipsis(tt.input, tt.max)
			if got != tt.want {
				t.Errorf("truncateWithEllipsis(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
			}
		})
	}
}

// TestTruncateWithEllipsisRuneCount verifies Bug 12A from TESTING-PLAN:
// the result must never exceed max runes. A mutation changing `m-3` to `m-2`
// in the cutoff calculation produces a result that is m+1 runes long.
func TestTruncateWithEllipsisRuneCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		max   int
	}{
		{"ascii", "abcdefghijk", 8},
		{"unicode", "héllo wörld extra", 9},
		{"exactly-max-plus-1", "abcde", 4},
		// Below ellipsis width: guard hard-truncates instead of emitting "...".
		{"below-ellipsis-2", "abcdef", 2},
		{"below-ellipsis-1", "abcdef", 1},
		{"below-ellipsis-0", "abcdef", 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := truncateWithEllipsis(tc.input, tc.max)
			runeCount := len([]rune(got))

			if runeCount > tc.max {
				t.Errorf("truncateWithEllipsis(%q, %d) = %q has %d runes, exceeds max %d (m-3→m-2 mutation?)",
					tc.input, tc.max, got, runeCount, tc.max)
			}
		})
	}
}

// TestTruncateWithEllipsisMinimumCase verifies Bug 12B from TESTING-PLAN:
// with m=4 and a 5-rune string, the result must be exactly 4 runes
// (1 content rune + "..."). The `>=` mutation would fire one rune too early,
// producing "..." with no content rune (3 runes) for a 4-rune input.
func TestTruncateWithEllipsisMinimumCase(t *testing.T) {
	t.Parallel()

	// 5-rune string, max=4 must yield "a...".
	got := truncateWithEllipsis("abcde", 4)
	runeCount := len([]rune(got))

	if runeCount != 4 {
		t.Errorf("truncateWithEllipsis(%q, 4) = %q has %d runes, want exactly 4", "abcde", got, runeCount)
	}

	if got != "a..." {
		t.Errorf("truncateWithEllipsis(%q, 4) = %q, want %q", "abcde", got, "a...")
	}
}

// TestIsPermanentSendErrSurvivesRetryDo is the regression test for the
// poison-drop classification bug: retry-go v5 strips the outer
// unrecoverableError marker before returning (unpackUnrecoverable), so
// classifying on IsRecoverable of the Do result always reported "transient"
// and permanent failures were requeued forever. The inner permanentError wrap
// must survive and classify correctly on both paths.
func TestIsPermanentSendErrSurvivesRetryDo(t *testing.T) {
	t.Parallel()

	permanent := errors.New("permanent 403")

	// Production pipeline shape: mark wraps, Do unpacks the outer layer.
	err := retry.New(
		retry.Attempts(3),
		retry.Delay(0),
	).Do(func() error {
		return retry.Unrecoverable(permanentError{permanent})
	})
	if !isPermanentSendErr(err) {
		t.Errorf("permanent error through retry.Do classified transient (marker stripped?): %v", err)
	}

	// Transient failure must stay recoverable.
	err = retry.New(
		retry.Attempts(2),
		retry.Delay(0),
	).Do(func() error {
		return permanent
	})
	if isPermanentSendErr(err) {
		t.Errorf("transient error through retry.Do misclassified permanent: %v", err)
	}
}

// TestIsPermanentSendErrDirect covers the non-retry.Do call sites (e.g.
// Discord UserChannelCreate) that classify markNamePermanent output directly.
func TestIsPermanentSendErrDirect(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("permanent")

	if !isPermanentSendErr(retry.Unrecoverable(permanentError{sentinel})) {
		t.Error("direct mark-wrapped error not classified permanent")
	}

	if isPermanentSendErr(sentinel) {
		t.Error("raw error misclassified as permanent")
	}

	if isPermanentSendErr(nil) {
		t.Error("nil misclassified as permanent")
	}
}

// TestMarkPermanentWrapping verifies each messenger's markNamePermanent wraps
// its permanent cases so isPermanentSendErr detects them, and leaves
// transient/unknown errors unwrapped.
func TestMarkPermanentWrapping(t *testing.T) {
	t.Parallel()

	unknown := errors.New("boom")

	cases := []struct {
		name      string
		permanent error
		mark      func(error) error
	}{
		{"telegram", bot.ErrorForbidden, markTelegramPermanent},
		{"whatsapp", whatsmeow.ErrNotLoggedIn, markWhatsAppPermanent},
		{"discord", &discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusForbidden}}, markDiscordPermanent},
		{"slack", slack.StatusCodeError{Code: http.StatusForbidden}, markSlackPermanent},
		{"calendar", &googleapi.Error{Code: http.StatusForbidden}, markCalendarPermanent},
		{"caldav", &caldavStatusError{code: http.StatusBadRequest}, markCalDAVPermanent},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.mark(tc.permanent); !isPermanentSendErr(got) {
				t.Errorf("mark(%v) = %v, want classified permanent", tc.permanent, got)
			}

			if got := tc.mark(unknown); isPermanentSendErr(got) {
				t.Errorf("mark(%v) = %v, want classified transient", unknown, got)
			}
		})
	}
}

// TestMarkCalDAVPermanentClasses: other 4xx will never succeed; 408, 423, 429
// and 5xx may. Redirects and 401/403/404 are config errors: not permanent
// (queued), yet they must still stop retry-go.
func TestMarkCalDAVPermanentClasses(t *testing.T) {
	t.Parallel()

	for code, wantPermanent := range map[int]bool{
		http.StatusFound:               false,
		http.StatusPermanentRedirect:   false,
		http.StatusPreconditionFailed:  true,
		http.StatusUnauthorized:        false,
		http.StatusForbidden:           false,
		http.StatusNotFound:            false,
		http.StatusRequestTimeout:      false,
		http.StatusTooManyRequests:     false,
		http.StatusLocked:              false,
		http.StatusInternalServerError: false,
		http.StatusBadGateway:          false,
	} {
		got := isPermanentSendErr(markCalDAVPermanent(&caldavStatusError{code: code}))
		if got != wantPermanent {
			t.Errorf("status %d permanent = %v, want %v", code, got, wantPermanent)
		}
	}

	for _, code := range []int{http.StatusFound, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound} {
		if retry.IsRecoverable(markCalDAVPermanent(&caldavStatusError{code: code})) {
			t.Errorf("status %d is recoverable; retrying cannot fix a config error", code)
		}
	}
}

// TestQueueAcceptsExamOnlyQueues: both calendar queues discard non-exams, so a
// spilled grade there sits unread until MaxQueueAge.
func TestQueueAcceptsExamOnlyQueues(t *testing.T) {
	t.Parallel()

	exam := msgtypes.Message{Code: msgtypes.Exam}
	grade := msgtypes.Message{Code: msgtypes.Grade}

	for _, q := range [][]byte{CalendarQueueName, CalDAVQueueName} {
		if !QueueAccepts(q, exam) {
			t.Errorf("%s rejected an exam", q)
		}

		if QueueAccepts(q, grade) {
			t.Errorf("%s accepted a grade it will never deliver", q)
		}
	}

	if !QueueAccepts(DiscordQueueName, grade) {
		t.Error("a general-purpose queue rejected a grade")
	}
}

// blockingLimiter never hands out a token, standing in for mail's minutes-long
// wait once its slack is spent.
type blockingLimiter struct{ release chan struct{} }

func (b blockingLimiter) Take() time.Time {
	<-b.release

	return time.Now()
}

// TestTakeCtxReturnsOnCancel: a shutdown must not wait out a rate-limit token.
// Take itself cannot be interrupted, and mail's can be minutes away — past a
// container's stop timeout, so the SIGKILL would lose the buffered backlog.
func TestTakeCtxReturnsOnCancel(t *testing.T) {
	t.Parallel()

	rl := blockingLimiter{release: make(chan struct{})}
	defer close(rl.release) // lets the abandoned Take goroutine finish

	ctx, cancel := context.WithCancel(t.Context())
	res := make(chan bool, 1)

	go func() { res <- takeCtx(ctx, rl) }()

	cancel()

	select {
	case got := <-res:
		if got {
			t.Error("takeCtx reported a token after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("takeCtx waited out the token despite a cancelled ctx")
	}

	if !takeCtx(t.Context(), ratelimit.NewUnlimited()) {
		t.Error("takeCtx refused a token on a live ctx")
	}
}

// TestMarkSlackAuthStopsRetrying: a bad token is bad for every attempt, so the
// loop must stop at once — while still queueing, not poison-dropping.
func TestMarkSlackAuthStopsRetrying(t *testing.T) {
	t.Parallel()

	got := markSlackPermanent(slack.SlackErrorResponse{Err: "invalid_auth"})
	if retry.IsRecoverable(got) {
		t.Error("invalid_auth is recoverable; retrying a bad token this cycle cannot succeed")
	}

	if isPermanentSendErr(got) {
		t.Error("invalid_auth is permanent; the alert would be dropped instead of queued")
	}
}

// TestAuthFailuresStopRetryingButQueue: a bad token or SMTP login is bad for
// every attempt and every recipient. The retry loop must stop at once, yet the
// alert must be queued: poison-dropping it would discard every already-flagged
// alert until the config is fixed and the bot restarted.
func TestAuthFailuresStopRetryingButQueue(t *testing.T) {
	t.Parallel()

	cases := map[string]error{
		"telegram": markTelegramPermanent(bot.ErrorUnauthorized),
		"discord":  markDiscordPermanent(&discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusUnauthorized}}),
		"slack":    markSlackPermanent(slack.SlackErrorResponse{Err: "invalid_auth"}),
	}

	for name, got := range cases {
		if retry.IsRecoverable(got) {
			t.Errorf("%s: auth failure is recoverable; retrying a bad credential this cycle cannot succeed", name)
		}

		if isPermanentSendErr(got) {
			t.Errorf("%s: auth failure is permanent; the alert would be dropped instead of queued", name)
		}
	}

	// Discord 403 is per user (DMs closed) and stays a poison-drop.
	if !isPermanentSendErr(markDiscordPermanent(&discordgo.RESTError{Response: &http.Response{StatusCode: http.StatusForbidden}})) {
		t.Error("discord 403 is no longer permanent; a user with closed DMs would retry every cycle")
	}

	for code, want := range map[int]bool{530: true, 534: true, 535: true, 550: false, 421: false} {
		err := fmt.Errorf("dial failed: %w", &textproto.Error{Code: code, Msg: "x"})
		if got := isMailAuthErr(err); got != want {
			t.Errorf("isMailAuthErr(%d) = %v, want %v", code, got, want)
		}
	}
}
