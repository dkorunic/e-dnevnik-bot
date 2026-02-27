package messenger

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"go.uber.org/ratelimit"
	_ "modernc.org/sqlite" // register pure-Go sqlite database/sql driver
)

func TestProcessWhatsApp(t *testing.T) {
	t.Parallel()
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

	eDB, err := sqlitedb.New(context.Background(), tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	processWhatsApp(context.Background(), eDB, msg, []string{"12345@s.whatsapp.net"}, rl, 1)
}

// TestWhatsAppLoginReturnsErrorOnConnectFailure verifies that whatsAppLogin propagates
// the Connect() error instead of always returning nil (Fix 1).
//
// NOTE: must not call t.Parallel() — os.Chdir is process-wide state.
// NOTE: this test requires that WhatsApp servers are NOT reachable (e.g., network-isolated
// CI). When run on a machine with internet access, Connect() succeeds and the test is
// skipped to avoid a false positive.
func TestWhatsAppLoginReturnsErrorOnConnectFailure(t *testing.T) {
	// Skip if the WhatsApp server is reachable — Connect() would succeed and we
	// cannot distinguish the pre-fix (swallows err) from the post-fix behaviour.
	conn, dialErr := net.DialTimeout("tcp", "web.whatsapp.com:443", 2*time.Second)
	if dialErr == nil {
		conn.Close()
		t.Skip("skipping: WhatsApp is reachable — Connect() would not fail in this environment")
	}

	tmpDir := t.TempDir()

	orig, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatal(err)
	}

	defer func() { _ = os.Chdir(orig) }()

	whatsAppCli = nil // reset so whatsAppLogin creates a fresh client

	err = whatsAppLogin(context.Background())
	// Connect() fails (no WhatsApp server). Before fix: returns nil. After fix: returns err.
	if err == nil {
		t.Error("whatsAppLogin should return a non-nil error when Connect() fails")
	}
}

// TestFilterGroupsByNameWorksOnUnsortedInput verifies that filterGroupsByName uses
// linear search (slices.Contains) instead of binary search, so unsorted config
// values still match correctly (Fix 2).
func TestFilterGroupsByNameWorksOnUnsortedInput(t *testing.T) {
	t.Parallel()

	// Group names as a user might write them in TOML — deliberately unsorted.
	wantGroups := []string{"Zeta", "Alpha", "Gamma"}

	jid1, _ := types.ParseJID("111111111111111111@g.us")
	jid2, _ := types.ParseJID("222222222222222222@g.us")
	jid3, _ := types.ParseJID("333333333333333333@g.us")

	joined := []*types.GroupInfo{
		{JID: jid1, GroupName: types.GroupName{Name: "Alpha"}},
		{JID: jid2, GroupName: types.GroupName{Name: "Beta"}}, // not in config
		{JID: jid3, GroupName: types.GroupName{Name: "Gamma"}},
	}

	got := filterGroupsByName(wantGroups, joined)
	if len(got) != 2 {
		t.Fatalf("expected 2 JIDs, got %d: %v", len(got), got)
	}

	gotSet := make(map[string]bool, len(got))
	for _, jid := range got {
		gotSet[jid] = true
	}

	if !gotSet[jid1.String()] {
		t.Errorf("expected jid1 (%v) in result", jid1.String())
	}

	if !gotSet[jid3.String()] {
		t.Errorf("expected jid3 (%v) in result", jid3.String())
	}

	if gotSet[jid2.String()] {
		t.Errorf("jid2 (Beta) should NOT be in result")
	}
}

func TestWhatsAppEventHandler(t *testing.T) {
	t.Parallel()
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
