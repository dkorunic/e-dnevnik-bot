// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

func oneMessage(g msgtypes.Message) <-chan msgtypes.Message {
	ch := make(chan msgtypes.Message, 1)
	ch <- g
	close(ch)

	return ch
}

func examToSend() msgtypes.Message {
	return msgtypes.Message{
		Code:      msgtypes.Exam,
		Username:  "u",
		Subject:   "s",
		Timestamp: time.Now().Add(72 * time.Hour),
		Fields:    []string{"x"},
	}
}

// A send that did not land has to reach exitWithError, or -t and a cron run
// report success for a bad password. Only a delivery or a deliberate skip is clean.
func TestCalDAVEntryReportsFailedSends(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		msg     msgtypes.Message
		status  int
		wantErr bool
	}{
		{name: "stored", msg: examToSend(), status: http.StatusCreated},
		{name: "already stored", msg: examToSend(), status: http.StatusPreconditionFailed},
		{name: "not an exam: skipped", msg: msgtypes.Message{Code: msgtypes.Grade}, status: http.StatusInternalServerError},
		{name: "permanent drop", msg: examToSend(), status: http.StatusBadRequest, wantErr: true},
		{name: "transient, queued", msg: examToSend(), status: http.StatusInternalServerError, wantErr: true},
		{name: "bad credentials, queued", msg: examToSend(), status: http.StatusUnauthorized, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			url, _ := caldavStub(t, tt.status)

			err := CalDAV(t.Context(), newRunTestDB(t), oneMessage(tt.msg), CalDAVConfig{URL: url, Username: "u", Password: "p", Retries: 1})
			if got := errors.Is(err, ErrSendFailed); got != tt.wantErr {
				t.Errorf("CalDAV() = %v, want ErrSendFailed: %v", err, tt.wantErr)
			}
		})
	}
}

// A shutdown cutting a send short is no failure: SIGTERM must still exit clean.
func TestCalDAVEntryCancelledIsNotAFailure(t *testing.T) {
	t.Parallel()

	url, _ := caldavStub(t, http.StatusInternalServerError)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if err := CalDAV(ctx, newRunTestDB(t), oneMessage(examToSend()), CalDAVConfig{URL: url, Username: "u", Password: "p", Retries: 1}); err != nil {
		t.Errorf("CalDAV() = %v, want nil on shutdown", err)
	}
}

// The case found on the real binary: -t with mail pointed at a closed port.
// Not parallel: Mail swaps the mailCli global.
func TestMailEntryReportsUnreachableServer(t *testing.T) {
	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v", err)
	}

	_, port, _ := net.SplitHostPort(ln.Addr().String())

	_ = ln.Close()

	mailMu.Lock()
	origCli, origCreds := mailCli, mailCreds
	mailMu.Unlock()

	t.Cleanup(func() {
		mailMu.Lock()
		mailCli, mailCreds = origCli, origCreds
		mailMu.Unlock()
	})

	portInt, _ := strconv.Atoi(port)

	err = Mail(t.Context(), mailTestDB(t), oneMessage(mailBatchMsg), MailConfig{
		Server: "127.0.0.1", Port: strconv.Itoa(portInt), Username: "u", Password: "p",
		From: "a@example.com", To: []string{"b@example.com"}, Retries: 1,
	})
	if !errors.Is(err, ErrSendFailed) {
		t.Errorf("Mail() = %v, want ErrSendFailed", err)
	}
}

func TestRecipientRunRecordsFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		act     func(*recipientRun)
		name    string
		wantErr bool
	}{
		{name: "delivered", act: func(r *recipientRun) { r.delivered("a") }},
		{name: "nothing to do", act: func(*recipientRun) {}},
		{name: "interrupted by shutdown", act: func(r *recipientRun) { r.interrupt() }},
		{name: "cancelled send", act: func(r *recipientRun) { r.failed(context.Canceled) }},
		{name: "transient", act: func(r *recipientRun) { r.failed(errors.New("boom")) }, wantErr: true},
		{name: "poisoned", act: func(r *recipientRun) { r.poison("a") }, wantErr: true},
		{name: "recipients it could not name", act: func(r *recipientRun) { r.unreached() }, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			eDB := newRunTestDB(t)
			run := newRecipientRun(msgtypes.Message{})

			tt.act(run)
			run.finish(t.Context(), eDB, SlackQueueName, msgtypes.Message{})

			if got := errors.Is(sendFailureErr(eDB, SlackQueueName), ErrSendFailed); got != tt.wantErr {
				t.Errorf("failure recorded = %v, want %v", got, tt.wantErr)
			}
		})
	}
}
