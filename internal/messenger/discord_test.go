// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"go.uber.org/ratelimit"
)

// discordTestClient returns an *http.Client whose transport rewrites every
// outbound request's host and scheme to match srvURL. This lets discordgo
// use its normal (hardcoded) endpoint variables while all traffic lands on
// the given httptest server.
func discordTestClient(srvURL string) *http.Client {
	u, _ := url.Parse(srvURL)

	return &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			r2 := req.Clone(req.Context())
			r2.URL.Scheme = u.Scheme
			r2.URL.Host = u.Host

			return http.DefaultTransport.RoundTrip(r2)
		}),
	}
}

// roundTripFunc is a functional implementation of http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// TestProcessDiscord must not run in parallel — it writes the package-level discordCli global.
func TestProcessDiscord(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"id": "12345"}`))
	}))
	defer server.Close()

	discordgo.EndpointAPI = server.URL + "/"
	s, err := discordgo.New("Bot token")
	if err != nil {
		t.Fatalf("Unable to create Discord session: %v", err)
	}
	discordCli = s

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

	processDiscord(context.Background(), eDB, msg, []string{"12345"}, rl, 1)
}

// TestProcessDiscordContinuesAfterChannelCreateFailure verifies that a channel
// creation failure for one recipient does not abort delivery to subsequent
// recipients (Fix 3: break → continue).
// NOTE: must not call t.Parallel() — discordCli and discordChannels are package-level globals.
func TestProcessDiscordContinuesAfterChannelCreateFailure(t *testing.T) {
	sentTo := make(map[string]bool)

	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "users/@me/channels") {
			var body struct {
				RecipientID string `json:"recipient_id"`
			}

			_ = json.NewDecoder(r.Body).Decode(&body)

			if body.RecipientID == "bad-user" {
				http.Error(w, `{"message":"forbidden"}`, http.StatusForbidden)

				return
			}

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"%s-chan"}`, body.RecipientID)

			return
		}

		if strings.Contains(r.URL.Path, "messages") {
			parts := strings.Split(r.URL.Path, "/")
			channelID := parts[len(parts)-2]

			mu.Lock()
			sentTo[channelID] = true
			mu.Unlock()
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"1"}`)
	}))

	defer srv.Close()

	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("unable to create Discord session: %v", err)
	}

	s.Client = discordTestClient(srv.URL)
	discordCli = s
	discordChannels = make(map[string]string)

	tmpDir := t.TempDir()

	eDB, err := sqlitedb.New(context.Background(), tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	defer eDB.Close()

	g := msgtypes.Message{Username: "u", Subject: "s", Fields: []string{"A"}, Descriptions: []string{"D"}}
	rl := ratelimit.New(1000)

	// "bad-user" fails channel creation; "good-user" must still receive.
	processDiscord(context.Background(), eDB, g, []string{"bad-user", "good-user"}, rl, 1)

	mu.Lock()
	defer mu.Unlock()

	if !sentTo["good-user-chan"] {
		t.Error("good-user should receive the message even when bad-user channel creation fails")
	}
}

// TestProcessDiscordSkipsAlreadyDeliveredRecipientsOnRetry verifies that
// recipients who received a message in the first attempt are not sent a
// duplicate when the queued message is retried (Fix 4: SkipRecipients).
// NOTE: must not call t.Parallel() — discordCli and discordChannels are package-level globals.
func TestProcessDiscordSkipsAlreadyDeliveredRecipientsOnRetry(t *testing.T) {
	sentTo := make(map[string]int)

	var mu sync.Mutex

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "users/@me/channels") {
			var body struct {
				RecipientID string `json:"recipient_id"`
			}

			_ = json.NewDecoder(r.Body).Decode(&body)

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"%s-chan"}`, body.RecipientID)

			return
		}

		if strings.Contains(r.URL.Path, "messages") {
			parts := strings.Split(r.URL.Path, "/")
			ch := parts[len(parts)-2]

			mu.Lock()
			sentTo[ch]++
			mu.Unlock()

			// Force fail-user to fail so the message gets queued.
			if ch == "fail-user-chan" {
				http.Error(w, `{"message":"error"}`, http.StatusInternalServerError)

				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"1"}`)
	}))

	defer srv.Close()

	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("unable to create Discord session: %v", err)
	}

	s.Client = discordTestClient(srv.URL)
	discordCli = s
	discordChannels = make(map[string]string)

	tmpDir := t.TempDir()

	eDB, err := sqlitedb.New(context.Background(), tmpDir)
	if err != nil {
		t.Fatal(err)
	}

	defer eDB.Close()

	g := msgtypes.Message{Username: "u", Subject: "s", Fields: []string{"A"}, Descriptions: []string{"D"}}
	rl := ratelimit.New(1000)
	userIDs := []string{"ok-user", "fail-user"}

	// First attempt: ok-user succeeds, fail-user fails → message queued with SkipRecipients=[ok-user].
	processDiscord(context.Background(), eDB, g, userIDs, rl, 1)

	failed := queue.FetchFailedMsgs(context.Background(), eDB, DiscordQueueName)
	if len(failed) != 1 {
		t.Fatalf("expected 1 queued message, got %d", len(failed))
	}

	// Reset channel cache to simulate a fresh process.
	discordChannels = make(map[string]string)

	// Second attempt using the queued message: ok-user must be skipped.
	processDiscord(context.Background(), eDB, failed[0].Msg, userIDs, rl, 1)

	mu.Lock()
	defer mu.Unlock()

	if sentTo["ok-user-chan"] != 1 {
		t.Errorf("ok-user should receive exactly 1 message total, got %d", sentTo["ok-user-chan"])
	}

	if sentTo["fail-user-chan"] < 2 {
		t.Errorf("fail-user should be retried (>= 2 attempts), got %d", sentTo["fail-user-chan"])
	}
}

// TestProcessDiscordStaleChannelRefreshed verifies that a permanent send
// failure against a cached DM channel ID (e.g. user closed the DM, Discord
// 404 Unknown Channel) evicts the cache entry, re-resolves the channel, and
// redelivers instead of poison-dropping the recipient.
// NOTE: must not call t.Parallel() — discordCli and discordChannels are package-level globals.
func TestProcessDiscordStaleChannelRefreshed(t *testing.T) {
	var (
		mu          sync.Mutex
		sentTo      = make(map[string]int)
		channelMade int
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "users/@me/channels") {
			mu.Lock()
			channelMade++
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"fresh-chan"}`)

			return
		}

		if strings.Contains(r.URL.Path, "messages") {
			parts := strings.Split(r.URL.Path, "/")
			ch := parts[len(parts)-2]

			mu.Lock()
			sentTo[ch]++
			mu.Unlock()

			if ch == "stale-chan" {
				// Discord's Unknown Channel error.
				http.Error(w, `{"message":"Unknown Channel","code":10003}`, http.StatusNotFound)

				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"1"}`)
	}))

	defer srv.Close()

	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("unable to create Discord session: %v", err)
	}

	s.Client = discordTestClient(srv.URL)
	discordCli = s
	discordChannels = map[string]string{"user1": "stale-chan"}

	eDB, err := sqlitedb.New(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer eDB.Close()

	g := msgtypes.Message{Username: "u", Subject: "s", Fields: []string{"A"}, Descriptions: []string{"D"}}
	rl := ratelimit.New(1000)

	processDiscord(context.Background(), eDB, g, []string{"user1"}, rl, 1)

	mu.Lock()
	defer mu.Unlock()

	if sentTo["fresh-chan"] != 1 {
		t.Errorf("message should be redelivered to the refreshed channel, got sends: %v", sentTo)
	}

	if channelMade != 1 {
		t.Errorf("expected exactly 1 channel re-resolution, got %d", channelMade)
	}

	if discordChannels["user1"] != "fresh-chan" {
		t.Errorf("cache should hold the refreshed channel, got %q", discordChannels["user1"])
	}

	if failed := queue.FetchFailedMsgs(context.Background(), eDB, DiscordQueueName); len(failed) != 0 {
		t.Errorf("nothing should be queued after a successful refresh, got %d", len(failed))
	}
}

// TestProcessDiscordEmptyFieldsPlaceholders verifies that empty scraped cells
// are replaced with a placeholder — Discord 400s on empty embed field
// name/value, which would otherwise poison-drop the whole alert.
// NOTE: must not call t.Parallel() — discordCli and discordChannels are package-level globals.
func TestProcessDiscordEmptyFieldsPlaceholders(t *testing.T) {
	var (
		mu      sync.Mutex
		payload []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "users/@me/channels") {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"chan1"}`)

			return
		}

		if strings.Contains(r.URL.Path, "messages") {
			mu.Lock()
			payload, _ = io.ReadAll(r.Body)
			mu.Unlock()
		}

		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"1"}`)
	}))

	defer srv.Close()

	s, err := discordgo.New("Bot test-token")
	if err != nil {
		t.Fatalf("unable to create Discord session: %v", err)
	}

	s.Client = discordTestClient(srv.URL)
	discordCli = s
	discordChannels = make(map[string]string)

	eDB, err := sqlitedb.New(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer eDB.Close()

	g := msgtypes.Message{Username: "u", Subject: "s", Fields: []string{""}, Descriptions: []string{""}}
	rl := ratelimit.New(1000)

	processDiscord(context.Background(), eDB, g, []string{"user1"}, rl, 1)

	mu.Lock()
	defer mu.Unlock()

	var sent struct {
		Embed  *discordgo.MessageEmbed   `json:"embed"`
		Embeds []*discordgo.MessageEmbed `json:"embeds"`
	}
	if err := json.Unmarshal(payload, &sent); err != nil {
		t.Fatalf("unable to decode sent payload %q: %v", payload, err)
	}

	embed := sent.Embed
	if embed == nil && len(sent.Embeds) > 0 {
		embed = sent.Embeds[0]
	}

	if embed == nil || len(embed.Fields) != 1 {
		t.Fatalf("expected 1 embed field, got %+v", embed)
	}

	if embed.Fields[0].Name == "" || embed.Fields[0].Value == "" {
		t.Errorf("empty embed field would be rejected by Discord: %+v", embed.Fields[0])
	}

	if failed := queue.FetchFailedMsgs(context.Background(), eDB, DiscordQueueName); len(failed) != 0 {
		t.Errorf("nothing should be queued after a successful send, got %d", len(failed))
	}
}

// TestDiscordInit must not run in parallel — discordInit() writes the package-level discordCli global.
func TestDiscordInit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	discordgo.EndpointGateway = server.URL
	err := discordInit("test-token")
	if err != nil {
		t.Fatalf("discordInit() error = %v", err)
	}
}
