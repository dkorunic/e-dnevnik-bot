// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"net"
	"slices"
	"strconv"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"go.uber.org/ratelimit"
)

// A server that cannot be reached fails every message alike, each one costing
// minutes of retry delay while the whole poll cycle waits. The alert is queued,
// and processMail reports it so the rest of the cycle queues unsent.
// Not parallel: swaps the mailCli global.
func TestProcessMailUnreachableServerStopsTheCycle(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}

	host, port, _ := net.SplitHostPort(ln.Addr().String())
	portInt, _ := strconv.Atoi(port)

	// Closed, so the dial is refused.
	_ = ln.Close()

	fakeSMTPClient(t, host, portInt)

	eDB := mailTestDB(t)

	tripped := processMail(t.Context(), eDB, mailBatchMsg, []string{"a@example.com"},
		"from@example.com", "subject", ratelimit.New(1000), 1)
	if !tripped {
		t.Error("processMail() = false, want the cycle stopped on an unreachable server")
	}

	got := queue.FetchFailedMsgs(t.Context(), eDB, MailQueueName)
	if len(got) != 1 || slices.Contains(got[0].Msg.SkipRecipients, "a@example.com") {
		t.Errorf("queued = %+v, want the alert queued for its recipient", got)
	}
}
