// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/go-telegram/bot"
	"go.uber.org/ratelimit"
)

// TestProcessTelegram must not run in parallel — it writes the package-level telegramCli global.
func TestProcessTelegram(t *testing.T) {
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

// TestProcessTelegramMigrateRemapsAndPersists verifies that a chat migrated
// to a supergroup is remapped in-process (message delivered to the new ID
// immediately), persisted to the config file, and followed on subsequent
// sends without another migrate round-trip.
// NOTE: must not run in parallel — it writes package-level telegramCli,
// telegramMigratedIDs and logger globals.
func TestProcessTelegramMigrateRemapsAndPersists(t *testing.T) {
	const (
		oldChatID = "12345"
		newChatID = "-1001234567890"
	)

	var (
		mu     sync.Mutex
		sentTo = map[int64]int{}
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if strings.HasSuffix(r.URL.Path, "/getMe") {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"test"}}`)

			return
		}

		// go-telegram/bot posts multipart form data.
		chatID, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)

		mu.Lock()
		sentTo[chatID]++
		mu.Unlock()

		if chatID == 12345 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: group chat was upgraded to a supergroup chat","parameters":{"migrate_to_chat_id":-1001234567890}}`)

			return
		}

		_, _ = io.WriteString(w, `{"ok":true,"result":{}}`)
	}))
	defer server.Close()

	b, err := bot.New("test-token", bot.WithServerURL(server.URL))
	if err != nil {
		t.Fatalf("Unable to create Telegram bot: %v", err)
	}
	telegramCli = b

	defer func() {
		telegramMigratedIDsMu.Lock()
		telegramMigratedIDs = map[string]string{}
		telegramMigratedIDsMu.Unlock()
	}()

	// Config file the remap is persisted to.
	confFile := t.TempDir() + "/config.toml"
	conf := "[[user]]\nusername = \"test@skole.hr\"\npassword = \"x\"\n\n" +
		"[telegram]\ntoken = \"123456789:AABBCCDDEEFFGGHHIIJJKKLLMMNNOOPPQQR\"\nchatids = [\"" + oldChatID + "\"]\n"

	if err := os.WriteFile(confFile, []byte(conf), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.WithValue(context.Background(), ConfFileKey, confFile)

	eDB, err := sqlitedb.New(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close() //nolint:errcheck

	msg := msgtypes.Message{
		Username:     "testuser",
		Subject:      "Test Subject",
		Descriptions: []string{"desc1"},
		Fields:       []string{"field1"},
	}

	rl := ratelimit.New(1000)

	// First send: migrate error, inline remap + resend to the supergroup.
	processTelegram(ctx, eDB, msg, []string{oldChatID}, rl, 1)

	// Second send: remap comes from the in-process map — no migrate round-trip.
	processTelegram(ctx, eDB, msg, []string{oldChatID}, rl, 1)

	mu.Lock()
	defer mu.Unlock()

	if sentTo[12345] != 1 {
		t.Errorf("old chat should see exactly one (migrate-failing) attempt, got %d", sentTo[12345])
	}

	if sentTo[-1001234567890] != 2 {
		t.Errorf("supergroup should receive both sends after remap, got %d", sentTo[-1001234567890])
	}

	telegramMigratedIDsMu.Lock()
	gotNew := telegramMigratedIDs[oldChatID]
	telegramMigratedIDsMu.Unlock()

	if gotNew != newChatID {
		t.Errorf("in-process remap = %q, want %q", gotNew, newChatID)
	}

	if failed := queue.FetchFailedMsgs(context.Background(), eDB, TelegramQueueName); len(failed) != 0 {
		t.Errorf("nothing should be queued after a successful remap, got %d", len(failed))
	}

	// The config file must carry the new chat ID.
	rewritten, err := config.LoadConfig(confFile)
	if err != nil {
		t.Fatalf("LoadConfig of rewritten config failed: %v", err)
	}

	if len(rewritten.Telegram.ChatIDs) != 1 || rewritten.Telegram.ChatIDs[0] != newChatID {
		t.Errorf("persisted chat IDs = %v, want [%q]", rewritten.Telegram.ChatIDs, newChatID)
	}
}

// TestTelegramInit must not run in parallel — telegramInit() writes the package-level telegramCli global.
func TestTelegramInit(t *testing.T) {
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
	err = telegramInit("test-token")
	if err != nil {
		t.Fatalf("telegramInit() error = %v", err)
	}
}

// TestProcessTelegramPoisonKeyedOnConfiguredChatID pins which identifier the
// SkipRecipients bookkeeping records once a chat has migrated to a supergroup.
//
// After a migration the loop sends to the *new* supergroup ID (u) while the
// configuration — and therefore the recipient list on every later retry — still
// holds the *original* ID. Recording the migrated ID in SkipRecipients means the
// skip never matches on retry, so the requeued message re-attempts a recipient
// it has already given up on, every cycle until MaxQueueAge.
//
// The migration is seeded directly and remapped to an unparseable ID so the
// poison happens in strconv.ParseInt, with no client or network involved: the
// point under test is the bookkeeping key, not how the failure arose. The second
// recipient plus a cancelled context is what forces the requeue so there is
// something to assert against.
// NOTE: must not call t.Parallel() — writes the package-level telegramMigratedIDs.
func TestProcessTelegramPoisonKeyedOnConfiguredChatID(t *testing.T) {
	const configuredID = "123456789"

	telegramMigratedIDsMu.Lock()
	orig := telegramMigratedIDs
	telegramMigratedIDs = map[string]string{configuredID: "migrated-and-broken"}
	telegramMigratedIDsMu.Unlock()

	t.Cleanup(func() {
		telegramMigratedIDsMu.Lock()
		telegramMigratedIDs = orig
		telegramMigratedIDsMu.Unlock()
	})

	eDB, err := sqlitedb.New(context.Background(), t.TempDir()+"/telegram-poison.db")
	if err != nil {
		t.Fatal(err)
	}

	defer eDB.Close() //nolint:errcheck

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	g := msgtypes.Message{
		Username:     "u",
		Subject:      "poison-after-migration",
		Descriptions: []string{"D"},
		Fields:       []string{"A"},
	}

	processTelegram(ctx, eDB, g, []string{configuredID, "987654321"}, ratelimit.New(1000), 1)

	failed := queue.FetchFailedMsgs(context.Background(), eDB, TelegramQueueName)
	if len(failed) != 1 {
		t.Fatalf("FetchFailedMsgs = %+v, want the message requeued once", failed)
	}

	if !slices.Contains(failed[0].Msg.SkipRecipients, configuredID) {
		t.Errorf("SkipRecipients = %v, want the *configured* chat ID %q; recording the migrated ID instead means the skip never matches the recipient list on retry",
			failed[0].Msg.SkipRecipients, configuredID)
	}
}
