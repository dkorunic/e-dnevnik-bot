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

// TestProcessWhatsApp must not run in parallel — it writes the package-level whatsAppCli global.
func TestProcessWhatsApp(t *testing.T) {
	// Smoke test only — full path needs a live WhatsApp connection.
	whatsAppCli = &whatsmeow.Client{}
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

	processWhatsApp(context.Background(), whatsAppCli, eDB, msg, []string{"12345@s.whatsapp.net"}, rl, 1)
}

// TestWhatsAppLoginReturnsErrorOnConnectFailure verifies that whatsAppLogin propagates
// the Connect() error instead of always returning nil (Fix 1).
//
// NOTE: must not call t.Parallel() — os.Chdir is process-wide state.
// NOTE: this test requires that WhatsApp servers are NOT reachable (e.g., network-isolated
// CI). When run on a machine with internet access, Connect() succeeds and the test is
// skipped to avoid a false positive.
func TestWhatsAppLoginReturnsErrorOnConnectFailure(t *testing.T) {
	// Skip when WhatsApp is reachable: Connect() succeeds and the test is meaningless.
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
	// Connect() must fail without a reachable server; pre-fix swallowed the err.
	if err == nil {
		t.Error("whatsAppLogin should return a non-nil error when Connect() fails")
	}
}

// TestFilterGroupsByNameMatchesConfiguredGroups verifies that filterGroupsByName
// returns the JIDs whose group names appear in the (config-sorted) wantGroups
// slice and excludes the rest. Production relies on slices.BinarySearch over a
// list pre-sorted by config.go (slices.SortFunc, see checkWhatsAppConf); this
// test passes wantGroups already sorted, mirroring that contract.
func TestFilterGroupsByNameMatchesConfiguredGroups(t *testing.T) {
	t.Parallel()

	// Sorted, matching the contract config loading enforces.
	wantGroups := []string{"Alpha", "Gamma", "Zeta"}

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

func TestIsWriteable(t *testing.T) {
	t.Parallel()

	if isWriteable("/non/existent/path/to/file.txt") {
		t.Error("isWriteable() returned true for non-existent path")
	}

	tmpfile, err := os.CreateTemp("", "test-writeable-*.txt")
	if err != nil {
		t.Fatal(err)
	}

	tmpfile.Close()
	defer os.Remove(tmpfile.Name())

	if !isWriteable(tmpfile.Name()) {
		t.Error("isWriteable() returned false for a writable file")
	}
}

// TestWhatsAppEventHandler must not run in parallel — it writes the package-level whatsAppCli global.
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
