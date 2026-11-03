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
	"sync/atomic"
	"testing"
	"time"

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
			say("250-ENHANCEDSTATUSCODES")
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

	return smtpReplyErrorAt(t, "RCPT TO", reply)
}

// smtpReplyErrorAt is smtpReplyError for the reply to a given SMTP command.
func smtpReplyErrorAt(t *testing.T, command, reply string) *mail.SendError {
	t.Helper()

	host, port := startFakeSMTP(t, smtpScript{reply: func(cmd string, _ int) string {
		if strings.HasPrefix(cmd, command) {
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
		{name: "5xx naming the mailbox", err: smtpReplyError(t, "550 5.1.1 no such user"), permanent: true},
		{name: "5xx about the mailbox", err: smtpReplyError(t, "552 5.2.2 mailbox full"), permanent: true},
		// A 5xx to RCPT TO proves nothing alone: Postfix delays sender and policy
		// rejections to this command by default.
		{name: "5xx about the sender", err: smtpReplyError(t, "553 5.7.1 sender address rejected"), permanent: false},
		{name: "5xx about the sender's address", err: smtpReplyError(t, "550 5.1.8 bad sender domain"), permanent: false},
		{name: "5xx with no status", err: smtpReplyError(t, "550 no such user"), permanent: false},
		{name: "4xx reply", err: smtpReplyError(t, "451 4.3.0 try later"), permanent: false},
		// Neither names the recipient, so neither may drop their alert.
		{name: "5xx to MAIL FROM", err: smtpReplyErrorAt(t, "MAIL FROM", "553 5.7.1 sender not owned"), permanent: false},
		{name: "5xx to DATA", err: smtpReplyErrorAt(t, "DATA", "554 5.7.1 blocked"), permanent: false},
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

// A 5xx to MAIL FROM is about the sending account (unverified sender, quota,
// block), so it holds for every recipient: dropping would lose each alert until
// the account is fixed. It is a config error, queued, stopping the cycle untried.
// Not parallel: swaps the mailCli global.
func TestProcessMailSenderRejectionIsQueuedAndStopsTheCycle(t *testing.T) {
	var mailFroms atomic.Int32

	host, port := startFakeSMTP(t, smtpScript{reply: func(cmd string, _ int) string {
		if strings.HasPrefix(cmd, "MAIL FROM") {
			mailFroms.Add(1)

			return "553 5.7.1 sender not owned"
		}

		return ""
	}})
	fakeSMTPClient(t, host, port)

	eDB := mailTestDB(t)

	tripped := processMail(t.Context(), eDB, mailBatchMsg, []string{"a@example.com", "b@example.com"},
		"from@example.com", "subject", ratelimit.New(1000), 3)
	if !tripped {
		t.Error("processMail() = false, want the cycle stopped on a rejected sender")
	}

	if got := mailFroms.Load(); got != 2 {
		t.Errorf("MAIL FROM sent %d times, want one attempt for each of 2 messages", got)
	}

	failed := queue.FetchFailedMsgs(t.Context(), eDB, MailQueueName)
	if len(failed) != 1 || len(failed[0].Msg.SkipRecipients) != 0 {
		t.Errorf("queued = %+v, want the alert queued for both recipients", failed)
	}
}

// A 5xx to DATA has unknown reach: one message's content, or the account. The
// alert is queued, and the cycle goes on in case it was the former.
// Not parallel: swaps the mailCli global.
func TestProcessMailDataRejectionIsQueued(t *testing.T) {
	var dataCmds atomic.Int32

	host, port := startFakeSMTP(t, smtpScript{reply: func(cmd string, _ int) string {
		if strings.HasPrefix(cmd, "DATA") {
			dataCmds.Add(1)

			return "554 5.7.1 blocked"
		}

		return ""
	}})
	fakeSMTPClient(t, host, port)
	setMailRetryDelay(t, time.Millisecond)

	eDB := mailTestDB(t)

	tripped := processMail(t.Context(), eDB, mailBatchMsg, []string{"a@example.com"},
		"from@example.com", "subject", ratelimit.New(1000), 3)
	if tripped {
		t.Error("processMail() = true, want the cycle to go on")
	}

	if got := dataCmds.Load(); got != 1 {
		t.Errorf("DATA sent %d times, want 1: a 5xx will not change on retry", got)
	}

	failed := queue.FetchFailedMsgs(t.Context(), eDB, MailQueueName)
	if len(failed) != 1 || len(failed[0].Msg.SkipRecipients) != 0 {
		t.Errorf("queued = %+v, want the alert queued for its recipient", failed)
	}
}

// go-mail never clears a message's send error, so a message that failed on
// attempt 1 still reads as attempted when attempt 2 cannot even log in. The bad
// login must be recognised and stop the retries: repeating it risks a lockout.
// Not parallel: swaps the mailCli global.
func TestSendMailBatchLoginFailureAfterMessageFailureStopsRetrying(t *testing.T) {
	var conns, logins atomic.Int32

	host, port := startFakeSMTP(t, smtpScript{reply: func(cmd string, _ int) string {
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			conns.Add(1)

			return "250-fake\r\n250 AUTH PLAIN"
		case strings.HasPrefix(cmd, "AUTH"):
			if conns.Load() > 1 {
				logins.Add(1)

				return "535 5.7.8 bad credentials"
			}

			return "235 ok"
		case strings.HasPrefix(cmd, "MAIL FROM"):
			return "451 4.3.0 try later"
		}

		return ""
	}})

	cli, err := mail.NewClient(host, mail.WithPort(port), mail.WithTLSPolicy(mail.NoTLS),
		mail.WithSMTPAuth(mail.SMTPAuthPlainNoEnc), mail.WithUsername("u"), mail.WithPassword("p"))
	if err != nil {
		t.Fatalf("mail.NewClient() = %v", err)
	}

	setMailCli(t, cli)
	setMailRetryDelay(t, time.Millisecond)

	m := mail.NewMsg()
	_ = m.From("from@example.com")
	_ = m.To("a@example.com")
	m.SetBodyString(mail.TypeTextPlain, "x")

	_, _, err = sendMailBatch(t.Context(), []*mail.Msg{m}, []string{"a@example.com"}, 4)
	if !isConfigSendErr(err) {
		t.Errorf("sendMailBatch() = %v, want a config error", err)
	}

	if got := logins.Load(); got != 1 {
		t.Errorf("rejected logins = %d, want 1: retrying a bad password risks a lockout", got)
	}
}

// Reading mailConnError from the aggregate finds the first matching attempt, so
// an unreachable server that came back with a 451 still stopped the whole cycle.
// Not parallel: swaps the mailCli global.
func TestProcessMailRecoveredConnectionDoesNotStopTheCycle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}

	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		first := true

		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}

			// Dropped before the banner: no SMTP spoken, so a mailConnError.
			if first {
				first = false

				_ = c.Close()

				continue
			}

			go serveFakeSMTP(c, smtpScript{reply: func(cmd string, _ int) string {
				if strings.HasPrefix(cmd, "MAIL FROM") {
					return "451 4.3.0 try later"
				}

				return ""
			}})
		}
	}()

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	portInt, _ := strconv.Atoi(port)

	fakeSMTPClient(t, host, portInt)
	setMailRetryDelay(t, time.Millisecond)

	tripped := processMail(t.Context(), mailTestDB(t), mailBatchMsg, []string{"a@example.com"},
		"from@example.com", "subject", ratelimit.New(1000), 2)
	if tripped {
		t.Error("processMail() = true, want the cycle to go on: the last attempt reached the server")
	}
}

// setMailCli swaps mailCli for the test. Not parallel-safe: it is a package global.
func setMailCli(t *testing.T, cli *mail.Client) {
	t.Helper()

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

// setMailRetryDelay shortens the wait between attempts for the test.
func setMailRetryDelay(t *testing.T, d time.Duration) {
	t.Helper()

	orig := mailRetryDelay
	mailRetryDelay = d

	t.Cleanup(func() { mailRetryDelay = orig })
}

// Only an enhanced status naming the recipient's address or mailbox drops an alert.
// The rest is queued, and only a login rejection stops the cycle: a 5.7.x can be
// one recipient's policy, which must not hold up everybody else's mail.
// Not parallel: swaps the mailCli global.
func TestProcessMailRecipientRejections(t *testing.T) {
	tests := []struct {
		name, reply      string
		dropped, tripped bool
	}{
		{name: "bad mailbox", reply: "550 5.1.1 no such user", dropped: true},
		{name: "disabled mailbox", reply: "550 5.2.1 mailbox disabled", dropped: true},
		{name: "sender rejected", reply: "553 5.7.1 Sender address rejected: not owned by user"},
		{name: "relay denied", reply: "554 5.7.1 Relay access denied"},
		{name: "bad sender domain", reply: "550 5.1.8 Sender address rejected"},
		{name: "no enhanced status", reply: "550 no such user"},
		{name: "login required", reply: "530 5.7.0 Authentication required", tripped: true},
		{name: "bad login", reply: "535 5.7.8 Authentication failed", tripped: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			host, port := startFakeSMTP(t, smtpScript{reply: func(cmd string, _ int) string {
				if strings.HasPrefix(cmd, "RCPT TO") {
					return tt.reply
				}

				return ""
			}})
			fakeSMTPClient(t, host, port)

			eDB := mailTestDB(t)

			tripped := processMail(t.Context(), eDB, mailBatchMsg, []string{"a@example.com"},
				"from@example.com", "subject", ratelimit.New(1000), 3)
			if tripped != tt.tripped {
				t.Errorf("processMail() = %v, want %v", tripped, tt.tripped)
			}

			failed := queue.FetchFailedMsgs(t.Context(), eDB, MailQueueName)

			if tt.dropped {
				if len(failed) != 0 {
					t.Errorf("queued = %+v, want the recipient dropped", failed)
				}

				return
			}

			if len(failed) != 1 || len(failed[0].Msg.SkipRecipients) != 0 {
				t.Errorf("queued = %+v, want the alert queued for its recipient", failed)
			}
		})
	}
}

// go-mail sends no RSET after a failed DATA, so a strict server answers the next
// message's MAIL FROM with a 503. That is about the connection's state, not the
// sender, so it must not read as a rejected account and stop the cycle.
// Not parallel: swaps the mailCli global.
func TestProcessMailNestedMailAfterDataRejectionIsNotAConfigError(t *testing.T) {
	var inTransaction atomic.Bool

	host, port := startFakeSMTP(t, smtpScript{reply: func(cmd string, _ int) string {
		switch {
		case strings.HasPrefix(cmd, "MAIL FROM"):
			if inTransaction.Swap(true) {
				return "503 5.5.1 Error: nested MAIL command"
			}
		case strings.HasPrefix(cmd, "RCPT TO"):
			return "250 OK"
		case strings.HasPrefix(cmd, "DATA"):
			return "554 5.7.1 blocked"
		}

		return ""
	}})
	fakeSMTPClient(t, host, port)

	eDB := mailTestDB(t)

	tripped := processMail(t.Context(), eDB, mailBatchMsg, []string{"a@example.com", "b@example.com"},
		"from@example.com", "subject", ratelimit.New(1000), 1)
	if tripped {
		t.Error("processMail() = true, want the cycle to go on: nothing says the account was rejected")
	}

	failed := queue.FetchFailedMsgs(t.Context(), eDB, MailQueueName)
	if len(failed) != 1 || len(failed[0].Msg.SkipRecipients) != 0 {
		t.Errorf("queued = %+v, want the alert queued for both recipients", failed)
	}
}

// One message's unknown-reach failure must not spare a recipient the server has
// named as dead in the same batch, or it is tried again every cycle.
// Not parallel: swaps the mailCli global.
func TestProcessMailDropsDeadRecipientBesideUnknownReachFailure(t *testing.T) {
	host, port := startFakeSMTP(t, smtpScript{reply: func(cmd string, _ int) string {
		switch {
		case strings.HasPrefix(cmd, "RCPT TO:<DEAD@"):
			return "550 5.1.1 no such user"
		case strings.HasPrefix(cmd, "DATA"):
			return "554 5.7.1 blocked"
		}

		return ""
	}})
	fakeSMTPClient(t, host, port)

	eDB := mailTestDB(t)

	processMail(t.Context(), eDB, mailBatchMsg, []string{"dead@example.com", "ok@example.com"},
		"from@example.com", "subject", ratelimit.New(1000), 1)

	failed := queue.FetchFailedMsgs(t.Context(), eDB, MailQueueName)
	if len(failed) != 1 {
		t.Fatalf("queued = %+v, want the alert queued for the other recipient", failed)
	}

	skip := failed[0].Msg.SkipRecipients
	if !slices.Contains(skip, "dead@example.com") || slices.Contains(skip, "ok@example.com") {
		t.Errorf("SkipRecipients = %v, want only the dead recipient", skip)
	}
}
