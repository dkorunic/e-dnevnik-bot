// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"net"
	"strconv"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/queue"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
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
