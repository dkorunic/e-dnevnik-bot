// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/go-telegram/bot"
)

// TestCredGuardUnrecordedIsChanged: nothing built yet means nothing to trust.
func TestCredGuardUnrecordedIsChanged(t *testing.T) {
	t.Parallel()

	var g credGuard

	if !g.changed("xoxb-token") {
		t.Error("an unrecorded guard reported unchanged; the first build would be skipped")
	}
}

func TestCredGuardSameCredsUnchanged(t *testing.T) {
	t.Parallel()

	var g credGuard

	g.record("xoxb-token")

	if g.changed("xoxb-token") {
		t.Error("identical credentials reported changed; every cycle would rebuild its client")
	}
}

func TestCredGuardRotatedCredsChanged(t *testing.T) {
	t.Parallel()

	var g credGuard

	g.record("xoxb-old")

	if !g.changed("xoxb-new") {
		t.Error("rotated credentials reported unchanged; the stale client would keep being served")
	}
}

// TestCredGuardAnyFieldChanges: mail keys on four fields, so any one counts.
func TestCredGuardAnyFieldChanges(t *testing.T) {
	t.Parallel()

	base := []string{"smtp.example.com", "587", "user", "secret"}

	for i := range base {
		rotated := append([]string(nil), base...)
		rotated[i] += "-rotated"

		var g credGuard

		g.record(base...)

		if !g.changed(rotated...) {
			t.Errorf("a change in field %d reported unchanged: %v vs %v", i, base, rotated)
		}
	}
}

// TestCredGuardFieldBoundariesAreDistinct: without a separator ("ab","c") and
// ("a","bc") collide, so a rotation that only shifts a boundary goes unnoticed.
func TestCredGuardFieldBoundariesAreDistinct(t *testing.T) {
	t.Parallel()

	var g credGuard

	g.record("ab", "c")

	if !g.changed("a", "bc") {
		t.Error("field boundaries collided; the digest is concatenating without a separator")
	}
}

// TestCredGuardRecordIsIdempotent: each successful build records again, so a
// repeat must not perturb the digest.
func TestCredGuardRecordIsIdempotent(t *testing.T) {
	t.Parallel()

	var g credGuard

	g.record("token")
	first := g.digest

	g.record("token")

	if g.digest != first {
		t.Error("re-recording identical credentials changed the digest")
	}
}

// TestDiscordInitRebuildsOnTokenChange: a DM channel belongs to the bot
// identity that opened it, so a rebuilt client must start with an empty cache
// or it posts into another bot's channels.
// Not parallel: writes discordCli and discordChannels.
func TestDiscordInitRebuildsOnTokenChange(t *testing.T) {
	discordMu.Lock()
	origCli, origCh := discordCli, discordChannels
	discordCli, discordChannels = nil, nil
	discordMu.Unlock()

	t.Cleanup(func() {
		discordMu.Lock()
		discordCli, discordChannels = origCli, origCh
		discordMu.Unlock()
	})

	if err := discordInit("first-token"); err != nil {
		t.Fatalf("discordInit() = %v", err)
	}

	discordMu.Lock()
	first := discordCli
	discordChannels["user-1"] = "channel-1"
	discordMu.Unlock()

	if err := discordInit("first-token"); err != nil {
		t.Fatalf("discordInit() unchanged = %v", err)
	}

	discordMu.Lock()
	same, keptCache := discordCli, len(discordChannels)
	discordMu.Unlock()

	if same != first || keptCache != 1 {
		t.Error("discordInit() rebuilt on unchanged credentials, discarding the resolved channel cache")
	}

	if err := discordInit("second-token"); err != nil {
		t.Fatalf("discordInit() after rotation = %v", err)
	}

	discordMu.Lock()
	rebuilt, cacheLen := discordCli, len(discordChannels)
	discordMu.Unlock()

	if rebuilt == first {
		t.Error("discordInit() kept the client built from the previous token")
	}

	if cacheLen != 0 {
		t.Errorf("discordInit() carried %d cached DM channel(s) across a bot identity change", cacheLen)
	}
}

// TestMailInitRebuildsOnAnyCredentialChange: mail keys on four fields, so a
// changed server or port matters as much as a rotated password.
// Not parallel: writes the package-level mailCli global.
func TestMailInitRebuildsOnAnyCredentialChange(t *testing.T) {
	mailMu.Lock()
	orig := mailCli
	mailCli = nil
	mailMu.Unlock()

	t.Cleanup(func() {
		mailMu.Lock()
		mailCli = orig
		mailMu.Unlock()
	})

	if err := mailInit("smtp.example.com", 587, "user", "secret"); err != nil {
		t.Fatalf("mailInit() = %v", err)
	}

	mailMu.Lock()
	first := mailCli
	mailMu.Unlock()

	if err := mailInit("smtp.example.com", 587, "user", "secret"); err != nil {
		t.Fatalf("mailInit() unchanged = %v", err)
	}

	mailMu.Lock()
	same := mailCli
	mailMu.Unlock()

	if same != first {
		t.Error("mailInit() rebuilt on unchanged credentials")
	}

	if err := mailInit("smtp.example.com", 587, "user", "rotated"); err != nil {
		t.Fatalf("mailInit() after rotation = %v", err)
	}

	mailMu.Lock()
	rebuilt := mailCli
	mailMu.Unlock()

	if rebuilt == first {
		t.Error("mailInit() kept the client built from the previous password")
	}
}

// TestTelegramInitSkipsRebuildOnUnchangedToken: bot.New does a network getMe,
// so this test hits the live API if the cache check regresses.
// Not parallel: writes the package-level telegramCli global.
func TestTelegramInitSkipsRebuildOnUnchangedToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true,"result":{}}`))
	}))
	defer server.Close()

	b, err := bot.New("seeded-token", bot.WithServerURL(server.URL))
	if err != nil {
		t.Fatalf("bot.New() = %v", err)
	}

	seedTelegramClient(t, b, "seeded-token")

	if err := telegramInit("seeded-token"); err != nil {
		t.Fatalf("telegramInit() unchanged = %v", err)
	}

	telegramMu.Lock()
	same := telegramCli
	telegramMu.Unlock()

	if same != b {
		t.Error("telegramInit() rebuilt on an unchanged token, spending a getMe round trip per cycle")
	}
}

// TestQueueUndeliveredSkipsRowsTheQueueCannotServe: an init failure drains the
// channel into the queue, but Calendar takes exams alone and its deferred stub
// never reads that queue — anything else sits there until MaxQueueAge.
func TestQueueUndeliveredSkipsRowsTheQueueCannotServe(t *testing.T) {
	t.Parallel()

	eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), "drain.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer eDB.Close() //nolint:errcheck

	ch := make(chan msgtypes.Message, 3)
	ch <- msgtypes.Message{Code: msgtypes.Exam, Username: "u", Subject: "exam"}
	ch <- msgtypes.Message{Code: msgtypes.Grade, Username: "u", Subject: "grade"}
	ch <- msgtypes.Message{Code: msgtypes.Reading, Username: "u", Subject: "reading"}
	close(ch)

	queueUndelivered(t.Context(), eDB, CalendarQueueName, ch)

	got := queue.FetchFailedMsgs(t.Context(), eDB, CalendarQueueName)
	if len(got) != 1 || got[0].Msg.Subject != "exam" {
		subjects := make([]string, 0, len(got))
		for _, q := range got {
			subjects = append(subjects, q.Msg.Subject)
		}

		t.Errorf("Calendar queue holds %v, want just the exam: the rest is unconsumable", subjects)
	}
}

// TestQueueUndeliveredKeepsEverythingForOtherQueues: the filter is Calendar's
// alone; a blanket drop would lose already dedup-flagged alerts.
func TestQueueUndeliveredKeepsEverythingForOtherQueues(t *testing.T) {
	t.Parallel()

	eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), "drain-all.db"))
	if err != nil {
		t.Fatal(err)
	}

	defer eDB.Close() //nolint:errcheck

	ch := make(chan msgtypes.Message, 2)
	ch <- msgtypes.Message{Code: msgtypes.Grade, Username: "u", Subject: "grade"}
	ch <- msgtypes.Message{Code: msgtypes.Reading, Username: "u", Subject: "reading"}
	close(ch)

	queueUndelivered(t.Context(), eDB, SlackQueueName, ch)

	if got := queue.FetchFailedMsgs(t.Context(), eDB, SlackQueueName); len(got) != 2 {
		t.Errorf("Slack queue holds %d messages, want both", len(got))
	}
}
