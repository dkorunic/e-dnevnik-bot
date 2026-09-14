// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"go.uber.org/ratelimit"
)

func init() {
	mailCli = nil // reset global for test isolation
}

// TestProcessMail must not run in parallel — mailInit() writes the package-level mailCli global.
func TestProcessMail(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Unable to start listener: %v", err)
	}
	defer ln.Close()

	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
	}()

	addr := ln.Addr().String()
	host, port, _ := net.SplitHostPort(addr)
	portInt, _ := strconv.Atoi(port)

	msg := msgtypes.Message{
		Username:     "testuser",
		Subject:      "Test Subject",
		Descriptions: []string{"desc1"},
		Fields:       []string{"field1"},
	}

	rl := ratelimit.New(1)

	eDB, err := sqlitedb.New(context.Background(), t.TempDir()+"/test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	_ = mailInit(host, portInt, "user", "pass")
	processMail(context.Background(), eDB, msg, []string{"test@example.com"}, "from@example.com", "subject", rl, 1)

	// Bogus listener can't complete SMTP; processMail must queue for retry.
	queued := queue.FetchFailedMsgs(context.Background(), eDB, MailQueueName)
	if len(queued) == 0 {
		t.Error("expected failed message in queue after unreachable SMTP send, got none")
	}
}

// TestMailTransportSecurity pins the two settings that decide whether SMTP
// credentials can cross the wire in the clear. AUTH PLAIN sends the password
// unencrypted, so TLSMandatory is what guarantees STARTTLS succeeded first;
// under TLSOpportunistic the client continues in plaintext against a server that
// does not advertise it, or one an attacker has stripped. The 587 fallback is
// the same concern: 25 aims a credentialed login at the inter-MTA relay port.
//
// Both are single-token changes with no behavioural symptom — mail still sends —
// so nothing but an explicit assertion notices.
// Not parallel: writes the package-level mailCli global.
func TestMailTransportSecurity(t *testing.T) {
	origCli := mailCli
	mailCli = nil

	t.Cleanup(func() { mailCli = origCli })

	eDB, err := sqlitedb.New(context.Background(), t.TempDir()+"/mail-tls.db")
	if err != nil {
		t.Fatal(err)
	}

	defer eDB.Close() //nolint:errcheck

	ch := make(chan msgtypes.Message)
	close(ch)

	// A non-numeric port drives the fallback; no message is sent, so the
	// unreachable server never matters.
	err = Mail(context.Background(), eDB, ch, MailConfig{
		Server:   "smtp.example.invalid",
		Port:     "not-a-number",
		Username: "user",
		Password: "pass",
		From:     "from@example.com",
		To:       []string{"to@example.com"},
		Retries:  1,
	})
	if err != nil {
		t.Fatalf("Mail() = %v, want nil", err)
	}

	if mailCli == nil {
		t.Fatal("Mail() did not initialise the client")
	}

	if got := mailCli.TLSPolicy(); got != "TLSMandatory" {
		t.Errorf("TLSPolicy() = %q, want %q — AUTH PLAIN must never traverse an unencrypted connection", got, "TLSMandatory")
	}

	if got := mailCli.ServerAddr(); got != "smtp.example.invalid:587" {
		t.Errorf("ServerAddr() = %q, want the submission port 587 — an unparseable port must not fall back to the relay port 25", got)
	}
}

// TestProcessMailInvalidFromIsMessageLevel: a From address go-mail rejects is a
// fault of the message, not of any recipient. Recording it per recipient
// poison-dropped every address and reported each one as the problem, which is
// how one bad config line read as a fleet of dead recipients.
// Not parallel: swaps the global logger to read its output.
func TestProcessMailInvalidFromIsMessageLevel(t *testing.T) {
	eDB, err := sqlitedb.New(context.Background(), t.TempDir()+"/mail-from.db")
	if err != nil {
		t.Fatal(err)
	}

	defer eDB.Close() //nolint:errcheck

	var buf bytes.Buffer

	origLogger := logger.Logger
	logger.Logger = logger.Output(&buf)

	t.Cleanup(func() { logger.Logger = origLogger })

	to := []string{"a@example.com", "b@example.com", "c@example.com"}

	processMail(context.Background(), eDB,
		msgtypes.Message{Code: msgtypes.Grade, Username: "u", Subject: "s", Fields: []string{"5"}},
		to, "not-an-address", "subject", ratelimit.NewUnlimited(), 1)

	// One message-level report, not one per recipient.
	if got := strings.Count(buf.String(), `"level":"error"`); got != 1 {
		t.Errorf("logged %d error lines for %d recipients, want 1: a bad From is one fault, not %d\n%s",
			got, len(to), len(to), buf.String())
	}

	// It must name the From address rather than blaming a recipient.
	if !strings.Contains(buf.String(), "not-an-address") {
		t.Errorf("the report does not name the offending From address:\n%s", buf.String())
	}

	// Unsendable for every recipient, so requeueing would churn until MaxQueueAge.
	if q := queue.FetchFailedMsgs(context.Background(), eDB, MailQueueName); len(q) != 0 {
		t.Errorf("FetchFailedMsgs = %+v, want nothing queued", q)
	}
}
