// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.uber.org/ratelimit"
)

// stubWhatsAppSender returns a scripted error per recipient JID, so the send
// path can be driven through outcomes a real client cannot reach without a
// paired device.
type stubWhatsAppSender struct {
	errs map[string]error
	sent []string
}

func (s *stubWhatsAppSender) SendMessage(_ context.Context, to types.JID, _ *waE2E.Message,
	_ ...whatsmeow.SendRequestExtra,
) (whatsmeow.SendResponse, error) {
	s.sent = append(s.sent, to.String())

	return whatsmeow.SendResponse{}, s.errs[to.String()]
}

// stubGroupLister returns a fixed set of joined groups.
type stubGroupLister struct {
	groups []*types.GroupInfo
	err    error
	calls  int
}

func (s *stubGroupLister) GetJoinedGroups(_ context.Context) ([]*types.GroupInfo, error) {
	s.calls++

	return s.groups, s.err
}

// resetWhatsAppGroupCache clears the process-wide group-resolution cache and
// restores it afterwards. The cache is keyed by nothing — it is a single set of
// globals — so tests that touch it must not run in parallel.
func resetWhatsAppGroupCache(t *testing.T) {
	t.Helper()

	whatsAppGroupsMu.Lock()
	origResolved, origAt, origComplete := whatsAppGroupsResolved, whatsAppGroupsResolvedAt, whatsAppGroupsComplete
	origIDs, origWarned, origPersisted := whatsAppResolvedUserIDs, whatsAppGroupsWarned, whatsAppGroupsPersisted
	origPartial := whatsAppGroupsPartial
	whatsAppGroupsResolved, whatsAppGroupsComplete, whatsAppResolvedUserIDs, whatsAppGroupsWarned = false, false, nil, ""
	whatsAppGroupsPartial = false
	whatsAppGroupsMu.Unlock()

	t.Cleanup(func() {
		whatsAppGroupsMu.Lock()
		whatsAppGroupsResolved, whatsAppGroupsResolvedAt, whatsAppGroupsComplete = origResolved, origAt, origComplete
		whatsAppResolvedUserIDs, whatsAppGroupsWarned, whatsAppGroupsPersisted = origIDs, origWarned, origPersisted
		whatsAppGroupsPartial = origPartial
		whatsAppGroupsMu.Unlock()
	})
}

// TestWhatsAppPoisonedRecipientRecordedInSkipRecipients: ErrUnknownServer is a
// "request is impossible" sentinel, so the recipient is dropped rather than
// requeued — but it must still reach SkipRecipients, or every retry re-attempts
// it until MaxQueueAge. The transient second recipient forces the requeue.
// Not parallel: reads package-level client globals.
func TestWhatsAppPoisonedRecipientRecordedInSkipRecipients(t *testing.T) {
	const (
		dead  = "111@s.whatsapp.net"
		flaky = "222@s.whatsapp.net"
	)

	cli := &stubWhatsAppSender{errs: map[string]error{
		dead:  whatsmeow.ErrUnknownServer,
		flaky: errors.New("websocket disconnected"),
	}}

	eDB, err := sqlitedb.New(context.Background(), filepath.Join(t.TempDir(), "wa-poison.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	g := msgtypes.Message{
		Username:     "u",
		Subject:      "wa-poison",
		Descriptions: []string{"D"},
		Fields:       []string{"A"},
	}

	processWhatsApp(context.Background(), cli, eDB, g, []string{dead, flaky}, false, ratelimit.New(1000), 1)

	failed := queue.FetchFailedMsgs(context.Background(), eDB, WhatsAppQueueName)
	if len(failed) != 1 {
		t.Fatalf("FetchFailedMsgs = %+v, want the message requeued for the transient failure", failed)
	}

	if !slices.Contains(failed[0].Msg.SkipRecipients, dead) {
		t.Errorf("SkipRecipients = %v, want the permanently-failed recipient recorded; otherwise it is re-attempted every cycle until MaxQueueAge",
			failed[0].Msg.SkipRecipients)
	}
}

// TestWhatsAppEffectiveUserIDsDropsCacheOnZeroMatches: no matching group means
// the bot was removed or the name is a typo. That must count as a failed
// resolution — caching an unchanged list would keep sending to groups the bot
// has left, churning the queue until MaxQueueAge. Hence strictly greater, since
// equal length means nothing was appended.
// Not parallel: writes the package-level group-resolution cache.
func TestWhatsAppEffectiveUserIDsDropsCacheOnZeroMatches(t *testing.T) {
	resetWhatsAppGroupCache(t)

	userIDs := []string{"111@s.whatsapp.net"}

	// Joined, but not one the configuration names.
	other := types.JID{User: "9990001", Server: types.GroupServer}
	cli := &stubGroupLister{groups: []*types.GroupInfo{{JID: other, Name: "Some Other Group"}}}

	got, _ := whatsAppEffectiveUserIDs(context.Background(), cli, userIDs, []string{"Razred 5.a"})

	if !slices.Equal(got, userIDs) {
		t.Errorf("effective user IDs = %v, want the configured IDs unchanged", got)
	}

	whatsAppGroupsMu.Lock()
	resolved, cached := whatsAppGroupsResolved, whatsAppResolvedUserIDs
	whatsAppGroupsMu.Unlock()

	if resolved {
		t.Error("zero group matches were cached as a successful resolution; a stale cache keeps sending to groups the bot has left")
	}

	if cached != nil {
		t.Errorf("stale resolution %v was retained after zero matches", cached)
	}
}

// TestWhatsAppReconnectKeepsGroupFallback: KeepAliveTimeout and StreamReplaced
// are routine, and the group lookup right after one is the likeliest to fail.
// Wiping the cache on those events left that failed lookup nothing to fall back
// on, so the cycle's alerts reached no group and nothing was queued. The event
// must force a fresh lookup while keeping the cached JIDs as the fallback.
// Not parallel: writes the package-level group-resolution cache.
func TestWhatsAppReconnectKeepsGroupFallback(t *testing.T) {
	static := []string{"111@s.whatsapp.net"}
	cached := []string{"111@s.whatsapp.net", "9990001@g.us"}

	setGroupCache(t, cached, time.Now())

	whatsAppGroupsInvalidate()

	if !whatsAppGroupsNeedsResolution() {
		t.Fatal("a reconnect did not force a fresh group lookup")
	}

	lister := &stubGroupLister{err: errors.New("usync timeout")}

	got, pending := whatsAppEffectiveUserIDs(context.Background(), lister, static, []string{"Razred 5.a"})
	if lister.calls != 1 {
		t.Errorf("GetJoinedGroups called %d times, want 1", lister.calls)
	}

	if !slices.Equal(got, cached) || pending {
		t.Errorf("whatsAppEffectiveUserIDs() = %v, pending %v; want the cached resolution %v, not pending", got, pending, cached)
	}
}

// TestWhatsAppUnresolvedGroupsQueueMessage: a failed lookup with no cache (the
// first cycle after a restart), or one that matched nothing, left the groups out
// of the recipient list, so their alerts were neither sent nor queued — and a
// groups-only config "delivered" to nobody. The message must be queued for the
// groups, skipping the static recipients it did reach.
// Not parallel: writes the package-level group-resolution cache.
func TestWhatsAppUnresolvedGroupsQueueMessage(t *testing.T) {
	resetWhatsAppGroupCache(t)

	static := "111@s.whatsapp.net"
	failing := &stubGroupLister{err: errors.New("usync timeout")}
	other := &stubGroupLister{groups: []*types.GroupInfo{{JID: types.JID{User: "9990001", Server: types.GroupServer}, Name: "Some Other Group"}}}

	for _, tt := range []struct {
		lister  *stubGroupLister
		name    string
		userIDs []string
	}{
		{name: "lookup failed, with static recipients", lister: failing, userIDs: []string{static}},
		{name: "lookup failed, groups only", lister: failing},
		{name: "nothing matched, with static recipients", lister: other, userIDs: []string{static}},
		{name: "nothing matched, groups only", lister: other},
		{name: "empty list, groups only", lister: &stubGroupLister{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, pending := whatsAppEffectiveUserIDs(context.Background(), tt.lister, tt.userIDs, []string{"Razred 5.a"})
			if !pending {
				t.Fatalf("whatsAppEffectiveUserIDs() = %v, not pending; the unresolved groups would be skipped silently", got)
			}

			eDB, err := sqlitedb.New(context.Background(), filepath.Join(t.TempDir(), "wa-pending.db.sqlite"))
			if err != nil {
				t.Fatalf("sqlitedb.New() failed: %v", err)
			}

			defer eDB.Close() //nolint:errcheck

			g := msgtypes.Message{Username: "u", Subject: "wa-pending", Descriptions: []string{"D"}, Fields: []string{"A"}}

			processWhatsApp(context.Background(), &stubWhatsAppSender{}, eDB, g, got, pending, ratelimit.New(1000), 1)

			failed := queue.FetchFailedMsgs(context.Background(), eDB, WhatsAppQueueName)
			if len(failed) != 1 {
				t.Fatalf("FetchFailedMsgs = %+v, want the message queued for the unresolved groups", failed)
			}

			if want := tt.userIDs; !slices.Equal(failed[0].Msg.SkipRecipients, want) {
				t.Errorf("SkipRecipients = %v, want %v: reached recipients must not get it twice", failed[0].Msg.SkipRecipients, want)
			}
		})
	}
}

// TestWhatsAppPartialMatchQueuesForMissingGroups: a group the lookup could not
// place misses the alert like any other unresolved one, and the matched names
// reaching their groups does not change that. The message is queued for it,
// whatever a later lookup says, until it matches.
// Not parallel: writes the package-level group-resolution cache.
func TestWhatsAppPartialMatchQueuesForMissingGroups(t *testing.T) {
	resetWhatsAppGroupCache(t)

	const (
		static = "111@s.whatsapp.net"
		found  = "9990001@g.us"
		late   = "9990002@g.us"
	)

	groups := []string{"Razred 5.a", "Razred 5.b"}
	groupA := &types.GroupInfo{JID: types.JID{User: "9990001", Server: types.GroupServer}, Name: "Razred 5.a"}
	groupB := &types.GroupInfo{JID: types.JID{User: "9990002", Server: types.GroupServer}, Name: "Razred 5.b"}

	lookup := func(lister *stubGroupLister) ([]string, bool) {
		return whatsAppEffectiveUserIDs(context.Background(), lister, []string{static}, groups)
	}

	got, pending := lookup(&stubGroupLister{groups: []*types.GroupInfo{groupA}})
	if !pending {
		t.Fatalf("partial match = %v, not pending; the missing group's alert would be lost", got)
	}

	eDB, err := sqlitedb.New(context.Background(), filepath.Join(t.TempDir(), "wa-partial.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() failed: %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	g := msgtypes.Message{Username: "u", Subject: "wa-partial", Descriptions: []string{"D"}, Fields: []string{"A"}}

	cli := &stubWhatsAppSender{}
	processWhatsApp(context.Background(), cli, eDB, g, got, pending, ratelimit.New(1000), 1)

	if want := []string{static, found}; !slices.Equal(cli.sent, want) {
		t.Errorf("sent to %v, want %v", cli.sent, want)
	}

	failed := queue.FetchFailedMsgs(context.Background(), eDB, WhatsAppQueueName)
	if len(failed) != 1 {
		t.Fatalf("FetchFailedMsgs = %+v, want the message queued for the missing group", failed)
	}

	// The cached fallbacks serve the same partial list, so they stay pending.
	if _, pending := lookup(&stubGroupLister{err: errors.New("usync timeout")}); !pending {
		t.Error("a failed lookup over a partial cache is not pending")
	}

	if _, pending := lookup(&stubGroupLister{}); !pending {
		t.Error("an empty list over a partial cache is not pending")
	}

	// The group turns up: the queued row reaches it and nobody else twice.
	got, pending = lookup(&stubGroupLister{groups: []*types.GroupInfo{groupA, groupB}})
	if pending {
		t.Errorf("full match = %v, still pending", got)
	}

	cli = &stubWhatsAppSender{}
	processWhatsApp(context.Background(), cli, eDB, failed[0].Msg, got, pending, ratelimit.New(1000), 1)

	if want := []string{late}; !slices.Equal(cli.sent, want) {
		t.Errorf("resend went to %v, want only %v", cli.sent, want)
	}
}
