package messenger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
	"github.com/go-telegram/bot"
	"go.uber.org/ratelimit"
)

func TestProcessTelegram(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()

	opts := []bot.Option{
		bot.WithServerURL(server.URL),
	}
	b, err := bot.New("test-token", opts...)
	if err != nil {
		t.Fatalf("Unable to create Telegram bot: %v", err)
	}
	telegramCli = b

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

	eDB, err := sqlitedb.New(context.Background(), tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	processTelegram(context.Background(), eDB, msg, []string{"12345"}, rl, 1)
}

func TestTelegramInit(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()

	opts := []bot.Option{
		bot.WithServerURL(server.URL),
	}
	b, err := bot.New("test-token", opts...)
	if err != nil {
		t.Fatalf("Unable to create Telegram bot: %v", err)
	}
	telegramCli = b
	err = telegramInit(context.Background(), "test-token")
	if err != nil {
		t.Fatalf("telegramInit() error = %v", err)
	}
}
