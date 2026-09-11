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
	"slices"
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

// TestProcessDiscordOmitsAllPaddingFields covers an all-blank message: the embed
// carries no fields rather than a row of "-" placeholders, and the send still
// succeeds. A title-only embed is valid; an empty name or value is not.
// NOTE: must not call t.Parallel() — discordCli and discordChannels are package-level globals.
func TestProcessDiscordOmitsAllPaddingFields(t *testing.T) {
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

	// A value-less column is cellValues' alignment padding, so it is skipped
	// rather than rendered as "-" — matching every other backend. A title-only
	// embed is valid Discord; what it must never contain is a field with an
	// empty name or value, which 400s and poison-drops the whole alert.
	if embed == nil {
		t.Fatalf("no embed sent")
	}

	if len(embed.Fields) != 0 {
		t.Errorf("expected no embed fields for an all-padding message, got %+v", embed.Fields)
	}

	for _, f := range embed.Fields {
		if f.Name == "" || f.Value == "" {
			t.Errorf("empty embed field would be rejected by Discord: %+v", f)
		}
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

// TestProcessDiscordPoisonedRecipientIsSkippedOnRetry: a 403 means the recipient
// will never accept the message, so it is dropped rather than requeued — but it
// must still be recorded in SkipRecipients. Omitting it costs a guaranteed 403
// per message per cycle for the 30 days of MaxQueueAge, against an API that
// rate-limits and bans for abuse.
//
// The mix matters: the transient failure is what forces the requeue while there
// is still a poisoned ID to carry.
// NOTE: must not call t.Parallel() — discordCli and discordChannels are package-level globals.
func TestProcessDiscordPoisonedRecipientIsSkippedOnRetry(t *testing.T) {
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

			switch ch {
			case "blocked-user-chan":
				// Permanent: bot blocked by this user.
				http.Error(w, `{"message":"cannot send messages to this user"}`, http.StatusForbidden)
			case "flaky-user-chan":
				// Transient: forces the message to be requeued.
				http.Error(w, `{"message":"server error"}`, http.StatusInternalServerError)
			default:
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"1"}`)
			}

			return
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

	origCli, origChannels := discordCli, discordChannels
	discordCli = s
	discordChannels = make(map[string]string)

	t.Cleanup(func() { discordCli, discordChannels = origCli, origChannels })

	eDB, err := sqlitedb.New(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	defer eDB.Close() //nolint:errcheck

	g := msgtypes.Message{Username: "u", Subject: "s", Fields: []string{"A"}, Descriptions: []string{"D"}}

	processDiscord(context.Background(), eDB, g, []string{"blocked-user", "flaky-user"}, ratelimit.New(1000), 1)

	failed := queue.FetchFailedMsgs(context.Background(), eDB, DiscordQueueName)
	if len(failed) != 1 {
		t.Fatalf("expected the message requeued once for the transient failure, got %d", len(failed))
	}

	if !slices.Contains(failed[0].Msg.SkipRecipients, "blocked-user") {
		t.Errorf("SkipRecipients = %v, want it to contain the poisoned recipient; otherwise every retry re-attempts a recipient that can never accept the message, for the whole of MaxQueueAge",
			failed[0].Msg.SkipRecipients)
	}
}

// TestProcessDiscordSkipsPaddingCells keeps Discord in step with the other
// backends: cellValues pads blank cells for alignment and the formatters skip
// them, but Discord rendered them as "-" fields. The substitution still guards
// the name, which Discord also 400s on.
func TestProcessDiscordSkipsPaddingCells(t *testing.T) {
	t.Parallel()

	g := msgtypes.Message{
		Code:         msgtypes.Grade,
		Username:     "testuser",
		Subject:      "Matematika",
		Descriptions: []string{"Datum", "Bilješka", "Element vrednovanja", "Ocjena"},
		Fields:       []string{"9.9.", "Treba pisati postupak", "", ""},
	}

	fields := discordEmbedFields(g, "Nova ocjena")

	if len(fields) != 2 {
		got := make([]string, 0, len(fields))
		for _, f := range fields {
			got = append(got, f.Name+"="+f.Value)
		}

		t.Fatalf("embed carried %d fields (%v), want only the 2 columns the portal filled", len(fields), got)
	}

	for _, f := range fields {
		if f.Value == "-" {
			t.Errorf("field %q rendered a padding placeholder", f.Name)
		}
	}
}

// TestDiscordEmbedFieldCapCountsEmittedFields pins what DiscordMaxFields counts.
// Spending cap budget on skipped padding loses real values that had room.
func TestDiscordEmbedFieldCapCountsEmittedFields(t *testing.T) {
	t.Parallel()

	var desc, vals []string

	// 30 columns, every even one blank padding: 15 real values, cap is 25.
	for i := range 30 {
		desc = append(desc, fmt.Sprintf("col%02d", i))

		if i%2 == 0 {
			vals = append(vals, "")
		} else {
			vals = append(vals, fmt.Sprintf("v%02d", i))
		}
	}

	g := msgtypes.Message{
		Code: msgtypes.Grade, Username: "u", Subject: "s",
		Descriptions: desc, Fields: vals,
	}

	fields := discordEmbedFields(g, "title")

	if len(fields) != 15 {
		t.Errorf("emitted %d fields, want all 15 non-blank values — %d cap slots were free",
			len(fields), DiscordMaxFields)
	}
}
