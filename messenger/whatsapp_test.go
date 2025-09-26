package messenger

import (
	"context"
	"os"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/ratelimit"
	_ "modernc.org/sqlite"
)

func TestProcessWhatsApp(t *testing.T) {
	// This is difficult to unit test without a live connection.
	// We will just call the function to ensure it doesn't panic.
	whatsAppCli = &whatsmeow.Client{}
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

	eDB, err := db.New(tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	processWhatsApp(context.Background(), eDB, msg, []string{"12345@s.whatsapp.net"}, rl, 1)
}

func TestWhatsAppEventHandler(t *testing.T) {
	container, err := sqlstore.New(context.Background(), "sqlite", "file::memory:?_pragma=foreign_keys(1)&_pragma=busy_timeout=10000", nil)
	if err != nil {
		t.Fatalf("failed to create sqlstore: %v", err)
	}
	device, err := container.GetFirstDevice(context.Background())
	if err != nil {
		t.Fatalf("failed to get device: %v", err)
	}
	whatsAppCli = whatsmeow.NewClient(device, nil)
	evt := &events.Connected{}
	whatsAppEventHandler(evt)
}
