// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

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
	origResolved, origAt := whatsAppGroupsResolved, whatsAppGroupsResolvedAt
	origIDs, origWarned := whatsAppResolvedUserIDs, whatsAppGroupsWarned
	whatsAppGroupsResolved, whatsAppResolvedUserIDs, whatsAppGroupsWarned = false, nil, false
	whatsAppGroupsMu.Unlock()

	t.Cleanup(func() {
		whatsAppGroupsMu.Lock()
		whatsAppGroupsResolved, whatsAppGroupsResolvedAt = origResolved, origAt
		whatsAppResolvedUserIDs, whatsAppGroupsWarned = origIDs, origWarned
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

	processWhatsApp(context.Background(), cli, eDB, g, []string{dead, flaky}, ratelimit.New(1000), 1)

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

	got := whatsAppEffectiveUserIDs(context.Background(), cli, userIDs, []string{"Razred 5.a"})

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
