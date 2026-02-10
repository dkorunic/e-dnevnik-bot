package messenger

import (
	"context"
	"net"
	"os"
	"strconv"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
	"go.uber.org/ratelimit"
)

func TestProcessMail(t *testing.T) {
	t.Parallel()
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

	// Create a temporary database for testing.
	tmpdir, err := os.MkdirTemp("", "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	eDB, err := sqlitedb.New(tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	processMail(context.Background(), eDB, msg, host, portInt, "user", "pass", []string{"test@example.com"}, "from@example.com", "subject", rl, 1)
}
