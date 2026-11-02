// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"go.uber.org/ratelimit"
)

// Past the limit a message waits in the queue for the next cycle: late, not
// failed, so it must not read as one.
func TestCycleSenderLimitQueuesTheRest(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)
	cs := cycleSender{limit: 2}

	var sent []string

	for _, s := range []string{"one", "two", "three", "four"} {
		cs.send(t.Context(), eDB, MailQueueName, msgtypes.Message{Subject: s}, func() bool {
			sent = append(sent, s)

			return false
		})
	}

	if !slices.Equal(sent, []string{"one", "two"}) {
		t.Errorf("sent = %v, want the first two", sent)
	}

	if got := queuedSubjects(t, eDB, MailQueueName); !slices.Equal(got, []string{"three", "four"}) {
		t.Errorf("queued = %v, want the rest in order", got)
	}

	if err := sendFailureErr(eDB, MailQueueName); err != nil {
		t.Errorf("sendFailureErr() = %v, want nil: nothing failed", err)
	}
}

// The ticker re-arms before a cycle, so a cycle shorter than the interval costs
// the next poll nothing. The shortest interval is 1h less 10% jitter, and each
// message but the first waits for a rate-limit token.
func TestMailCycleLimitFitsTheShortestPollInterval(t *testing.T) {
	t.Parallel()

	if MailCycleLimit < 1 {
		t.Fatalf("MailCycleLimit = %d, mail would never send", MailCycleLimit)
	}

	if hold := (MailCycleLimit - 1) * MailMinDelay; hold >= 54*time.Minute {
		t.Errorf("a full cycle of mail waits %v on the limiter, past the shortest poll interval", hold)
	}
}

func TestProcessCalendarRecordsFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		wantErr bool
	}{
		{name: "inserted", status: http.StatusOK},
		{name: "already there", status: http.StatusConflict},
		{name: "permanent drop", status: http.StatusBadRequest, wantErr: true},
		{name: "transient, queued", status: http.StatusInternalServerError, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eDB := calendarTestDB(t)
			svc, _ := calendarStub(t, tt.status, `{}`)

			processCalendar(t.Context(), eDB, examToSend(), ratelimit.NewUnlimited(), svc, "primary", 1)

			if got := errors.Is(sendFailureErr(eDB, CalendarQueueName), ErrSendFailed); got != tt.wantErr {
				t.Errorf("failure recorded = %v, want %v", got, tt.wantErr)
			}
		})
	}
}

func TestProcessCalendarCancelledIsNotAFailure(t *testing.T) {
	t.Parallel()

	eDB := calendarTestDB(t)
	svc, _ := calendarStub(t, http.StatusInternalServerError, `{}`)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	processCalendar(ctx, eDB, examToSend(), ratelimit.NewUnlimited(), svc, "primary", 1)

	if err := sendFailureErr(eDB, CalendarQueueName); err != nil {
		t.Errorf("sendFailureErr() = %v, want nil on shutdown", err)
	}
}
