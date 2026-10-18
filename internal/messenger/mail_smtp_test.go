// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"bufio"
	"context"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	mail "github.com/wneessen/go-mail"
	"go.uber.org/ratelimit"
)

// smtpScript overrides the fake server's answers. reply sees each command line
// and the number of messages accepted so far; a non-empty answer replaces the
// default. hangupAfter closes the connection once that many messages are
// accepted, as a server dropping mid-batch does.
type smtpScript struct {
	reply       func(cmd string, accepted int) string
	hangupAfter int
}

// startFakeSMTP serves a minimal plaintext SMTP dialogue: no STARTTLS and no
// AUTH, which is why tests drive it through fakeSMTPClient rather than
// mailInit's mandatory-TLS client. It is real enough that go-mail builds its
// own SendErrors, whose server code is unexported and cannot be faked.
func startFakeSMTP(t *testing.T, script smtpScript) (string, int) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			go serveFakeSMTP(c, script)
		}
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	portInt, _ := strconv.Atoi(port)

	return host, portInt
}

func serveFakeSMTP(c net.Conn, script smtpScript) {
	defer c.Close()

	r := bufio.NewReader(c)
	say := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }

	say("220 fake ESMTP")

	accepted := 0

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}

		cmd := strings.ToUpper(strings.TrimSpace(line))

		if script.reply != nil {
			if ans := script.reply(cmd, accepted); ans != "" {
				say(ans)

				continue
			}
		}

		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			say("250-fake")
			say("250 8BITMIME")
		case strings.HasPrefix(cmd, "DATA"):
			say("354 go ahead")

			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}

				if l == ".\r\n" {
					break
				}
			}

			accepted++

			say("250 queued")

			if script.hangupAfter > 0 && accepted >= script.hangupAfter {
				return
			}
		case strings.HasPrefix(cmd, "QUIT"):
			say("221 bye")

			return
		default:
			say("250 OK")
		}
	}
}

// fakeSMTPClient swaps mailCli for a plaintext client aimed at the fake server.
// Not parallel-safe: mailCli is a package global.
func fakeSMTPClient(t *testing.T, host string, port int) {
	t.Helper()

	cli, err := mail.NewClient(host, mail.WithPort(port), mail.WithTLSPolicy(mail.NoTLS))
	if err != nil {
		t.Fatalf("mail.NewClient() = %v", err)
	}

	mailMu.Lock()
	orig := mailCli
	mailCli = cli
	mailMu.Unlock()

	t.Cleanup(func() {
		mailMu.Lock()
		mailCli = orig
		mailMu.Unlock()
	})
}

// smtpReplyError returns the SendError go-mail builds from a real server
// rejecting MAIL FROM with reply.
func smtpReplyError(t *testing.T, reply string) *mail.SendError {
	t.Helper()

	host, port := startFakeSMTP(t, smtpScript{reply: func(cmd string, _ int) string {
		if strings.HasPrefix(cmd, "MAIL FROM") {
			return reply
		}

		return ""
	}})

	cli, err := mail.NewClient(host, mail.WithPort(port), mail.WithTLSPolicy(mail.NoTLS))
	if err != nil {
		t.Fatalf("mail.NewClient() = %v", err)
	}

	m := mail.NewMsg()
	_ = m.From("from@example.com")
	_ = m.To("to@example.com")
	m.SetBodyString(mail.TypeTextPlain, "x")

	_ = cli.DialAndSendWithContext(t.Context(), m)

	sendErr, ok := errors.AsType[*mail.SendError](m.SendError())
	if !ok {
		t.Fatalf("no SendError for reply %q: %v", reply, m.SendError())
	}

	return sendErr
}

func mailTestDB(t *testing.T) *sqlitedb.Edb {
	t.Helper()

	eDB, err := sqlitedb.New(context.Background(), filepath.Join(t.TempDir(), "mail.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	return eDB
}

var mailBatchMsg = msgtypes.Message{
	Username:     "u",
	Subject:      "mail-batch",
	Descriptions: []string{"D"},
	Fields:       []string{"A"},
}

// TestProcessMailConnectionDroppedMidBatchRequeues: a server that hangs up
// after the first message fails the second with a transport error (EOF, a
// reset). go-mail judges temp by a leading '4' in the error text, so that
// reads as permanent, and the recipient was poison-dropped — an alert dedup
// had already flagged, lost for good. It must be requeued, skipping the
// recipient already served.
// Not parallel: swaps the mailCli global.
func TestProcessMailConnectionDroppedMidBatchRequeues(t *testing.T) {
	host, port := startFakeSMTP(t, smtpScript{hangupAfter: 1})
	fakeSMTPClient(t, host, port)

	eDB := mailTestDB(t)

	processMail(t.Context(), eDB, mailBatchMsg, []string{"a@example.com", "b@example.com"},
		"from@example.com", "subject", ratelimit.New(1000), 1)

	failed := queue.FetchFailedMsgs(t.Context(), eDB, MailQueueName)
	if len(failed) != 1 {
		t.Fatalf("FetchFailedMsgs = %+v, want the message requeued for the dropped connection", failed)
	}

	skip := failed[0].Msg.SkipRecipients
	if !slices.Contains(skip, "a@example.com") {
		t.Errorf("SkipRecipients = %v, want the delivered recipient, or the retry sends it twice", skip)
	}

	if slices.Contains(skip, "b@example.com") {
		t.Errorf("SkipRecipients = %v: the undelivered recipient was poison-dropped", skip)
	}
}

// TestProcessMail5xxRecipientIsDropped is the complement: a real 5xx reply can
// never succeed, so that recipient is poison-dropped, not requeued every cycle
// until MaxQueueAge.
// Not parallel: swaps the mailCli global.
func TestProcessMail5xxRecipientIsDropped(t *testing.T) {
	host, port := startFakeSMTP(t, smtpScript{reply: func(cmd string, _ int) string {
		if strings.HasPrefix(cmd, "RCPT TO:<B@") {
			return "550 5.1.1 no such user"
		}

		return ""
	}})
	fakeSMTPClient(t, host, port)

	eDB := mailTestDB(t)

	processMail(t.Context(), eDB, mailBatchMsg, []string{"a@example.com", "b@example.com"},
		"from@example.com", "subject", ratelimit.New(1000), 1)

	if failed := queue.FetchFailedMsgs(t.Context(), eDB, MailQueueName); len(failed) != 0 {
		t.Errorf("FetchFailedMsgs = %+v, want nothing queued: a 550 recipient is dropped and the other was delivered", failed)
	}
}

// TestIsPermanentMailSendErr pins the classification on real server replies.
// Not parallel: smtpReplyError's server is cheap, but keep the file uniform.
func TestIsPermanentMailSendErr(t *testing.T) {
	tests := []struct {
		err       *mail.SendError
		name      string
		permanent bool
	}{
		{name: "5xx reply", err: smtpReplyError(t, "550 5.7.1 rejected"), permanent: true},
		{name: "4xx reply", err: smtpReplyError(t, "451 4.3.0 try later"), permanent: false},
		// No server code: a transport failure, whatever IsTemp says.
		{name: "transport error", err: &mail.SendError{Reason: mail.ErrSMTPMailFrom}, permanent: false},
		{name: "connection check", err: &mail.SendError{Reason: mail.ErrConnCheck}, permanent: false},
		// go-mail's own refusals: the message cannot be addressed or encoded.
		{name: "unaddressable sender", err: &mail.SendError{Reason: mail.ErrGetSender}, permanent: true},
		{name: "unaddressable recipient", err: &mail.SendError{Reason: mail.ErrGetRcpts}, permanent: true},
	}

	for _, tt := range tests {
		if got := isPermanentMailSendErr(tt.err); got != tt.permanent {
			t.Errorf("%s: isPermanentMailSendErr(%v) = %v, want %v", tt.name, tt.err, got, tt.permanent)
		}
	}
}
