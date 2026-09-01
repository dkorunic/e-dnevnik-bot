// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"net"
	"strconv"
	"testing"

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
// credentials can ever cross the wire in the clear.
//
// The client authenticates with AUTH PLAIN, which transmits the username and
// password base64-encoded but unencrypted. TLSMandatory is what guarantees
// STARTTLS succeeded before that happens: under TLSOpportunistic the client
// silently continues in plaintext against a server that does not advertise
// STARTTLS — or one that has been stripped of it by an active attacker — and
// the mailbox password is disclosed.
//
// The port fallback matters for the same reason: 587 is the submission port,
// where authentication and STARTTLS are expected. Falling back to 25 aims a
// credentialed login at the inter-MTA relay port instead.
//
// Both are single-token changes with no behavioural symptom — mail still sends
// — so nothing but an explicit assertion will notice them.
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
