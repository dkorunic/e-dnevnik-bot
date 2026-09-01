// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/format"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// TestUndeliveredRecipients covers the set-difference used to retry only the
// recipients an SMTP batch failed to reach. Re-sending to an already-delivered
// address duplicates the alert, so order and exactness both matter.
func TestUndeliveredRecipients(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		rcpt      []string
		delivered []string
		want      []string
	}{
		{
			name:      "nothing delivered returns all in order",
			rcpt:      []string{"a@x", "b@x", "c@x"},
			delivered: nil,
			want:      []string{"a@x", "b@x", "c@x"},
		},
		{
			name:      "everything delivered returns nil",
			rcpt:      []string{"a@x", "b@x"},
			delivered: []string{"a@x", "b@x"},
			want:      nil,
		},
		{
			name:      "partial delivery preserves original order",
			rcpt:      []string{"a@x", "b@x", "c@x", "d@x"},
			delivered: []string{"c@x", "a@x"},
			want:      []string{"b@x", "d@x"},
		},
		{
			name:      "delivered entries not in rcpt are ignored",
			rcpt:      []string{"a@x"},
			delivered: []string{"zzz@x"},
			want:      []string{"a@x"},
		},
		{
			name:      "empty recipient list returns nil",
			rcpt:      nil,
			delivered: []string{"a@x"},
			want:      nil,
		},
		{
			name:      "duplicate recipients are both reported",
			rcpt:      []string{"a@x", "a@x"},
			delivered: nil,
			want:      []string{"a@x", "a@x"},
		},
		{
			name:      "match is exact, not case-insensitive",
			rcpt:      []string{"A@x"},
			delivered: []string{"a@x"},
			want:      []string{"A@x"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := undeliveredRecipients(tt.rcpt, tt.delivered)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("undeliveredRecipients(%v, %v) = %v, want %v", tt.rcpt, tt.delivered, got, tt.want)
			}
		})
	}
}

// pairs builds n description/grade pairs with predictable, uniform lengths so
// the truncation tests can reason about the budget.
func pairs(n int) ([]string, []string) {
	descriptions := make([]string, n)
	grade := make([]string, n)

	for i := range n {
		descriptions[i] = fmt.Sprintf("Ocjena broj %02d", i)
		grade[i] = fmt.Sprintf("%d", i%5+1)
	}

	return descriptions, grade
}

// TestTruncateHTMLBodyFitsBudget checks the primary contract across a sweep of
// budgets: the rendered message must fit maxRunes. The one documented exception
// is a budget too small even for the header, which returns header-only.
func TestTruncateHTMLBodyFitsBudget(t *testing.T) {
	t.Parallel()

	descriptions, grade := pairs(40)
	headerOnly := format.HTMLMsg("pero.peric", "Matematika", msgtypes.Grade, nil, nil)
	headerLen := utf8.RuneCountInString(headerOnly)

	for _, maxRunes := range []int{0, 10, headerLen - 1, headerLen, headerLen + 5, 120, 400, 4096} {
		t.Run(fmt.Sprintf("maxRunes=%d", maxRunes), func(t *testing.T) {
			t.Parallel()

			got := truncateHTMLBody("pero.peric", "Matematika", msgtypes.Grade, descriptions, grade, maxRunes)
			n := utf8.RuneCountInString(got)

			if n <= maxRunes {
				return
			}

			// Over budget is only acceptable as the header-only fallback.
			if got != headerOnly {
				t.Errorf("result is %d runes (budget %d) and is not the header-only fallback:\n%q", n, maxRunes, got)
			}
		})
	}
}

// TestTruncateHTMLBodyKeepsMaximumPairs pins the binary search's optimality,
// not merely that it fits: the result must hold as many description/grade pairs
// as the budget allows, and one more pair must overflow it. A search that
// returned a needlessly short message would silently discard grades.
func TestTruncateHTMLBodyKeepsMaximumPairs(t *testing.T) {
	t.Parallel()

	const (
		user    = "pero.peric"
		subject = "Matematika"
		total   = 40
	)

	descriptions, grade := pairs(total)

	for _, maxRunes := range []int{80, 150, 300, 700, 1500} {
		t.Run(fmt.Sprintf("maxRunes=%d", maxRunes), func(t *testing.T) {
			t.Parallel()

			got := truncateHTMLBody(user, subject, msgtypes.Grade, descriptions, grade, maxRunes)

			// Recover the pair count the result actually kept.
			kept := -1

			for n := range total + 1 {
				if format.HTMLMsg(user, subject, msgtypes.Grade, descriptions[:n], grade[:n]) == got {
					kept = n

					break
				}
			}

			if kept < 0 {
				t.Fatalf("result does not match any prefix rendering; truncation must trim the input, not the output:\n%q", got)
			}

			if utf8.RuneCountInString(got) > maxRunes {
				t.Fatalf("kept %d pairs at %d runes, over the %d budget", kept, utf8.RuneCountInString(got), maxRunes)
			}

			if kept == total {
				return
			}

			oneMore := format.HTMLMsg(user, subject, msgtypes.Grade, descriptions[:kept+1], grade[:kept+1])
			if utf8.RuneCountInString(oneMore) <= maxRunes {
				t.Errorf("kept only %d pairs but %d also fit within %d runes; the binary search dropped a grade needlessly",
					kept, kept+1, maxRunes)
			}
		})
	}
}

// TestTruncateHTMLBodyBalancedTags guards the reason the input is trimmed
// rather than the output: Telegram rejects a message whose HTML tags are
// unbalanced, so a truncated body must still close every tag it opens.
func TestTruncateHTMLBodyBalancedTags(t *testing.T) {
	t.Parallel()

	descriptions, grade := pairs(60)

	for _, maxRunes := range []int{5, 50, 100, 250, 800} {
		got := truncateHTMLBody("pero.peric", "Matematika", msgtypes.Grade, descriptions, grade, maxRunes)

		for _, tag := range []string{"b", "pre"} {
			open := strings.Count(got, "<"+tag+">")
			closed := strings.Count(got, "</"+tag+">")

			if open != closed {
				t.Errorf("maxRunes=%d: %d <%s> vs %d </%s> — Telegram's parser rejects unbalanced HTML:\n%q",
					maxRunes, open, tag, closed, tag, got)
			}
		}
	}
}

// TestTruncateHTMLBodyUsesMinLength: descriptions and grades are parallel
// slices scraped separately, so a length mismatch is possible. Pairing must
// stop at the shorter one rather than index out of range.
func TestTruncateHTMLBodyUsesMinLength(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		descriptions []string
		grade        []string
	}{
		{name: "more descriptions than grades", descriptions: []string{"a", "b", "c"}, grade: []string{"1"}},
		{name: "more grades than descriptions", descriptions: []string{"a"}, grade: []string{"1", "2", "3"}},
		{name: "both empty", descriptions: nil, grade: nil},
		{name: "descriptions only", descriptions: []string{"a", "b"}, grade: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := truncateHTMLBody("u", "s", msgtypes.Grade, tt.descriptions, tt.grade, 4096)
			if got == "" {
				t.Error("truncateHTMLBody returned an empty string; the header must always be present")
			}
		})
	}
}

// TestTruncateHTMLBodyCountsRunesNotBytes: Croatian subject names carry
// multi-byte characters. Budgeting in bytes would truncate far more
// aggressively than Telegram's rune-based limit requires.
func TestTruncateHTMLBodyCountsRunesNotBytes(t *testing.T) {
	t.Parallel()

	const n = 12

	descriptions := make([]string, n)
	grade := make([]string, n)

	for i := range n {
		descriptions[i] = "Ocjena iz čćžšđ"
		grade[i] = "5"
	}

	full := format.HTMLMsg("u", "Priroda i društvo", msgtypes.Grade, descriptions, grade)
	runes := utf8.RuneCountInString(full)

	if len(full) <= runes {
		t.Fatal("fixture is not multi-byte; the test would not distinguish runes from bytes")
	}

	// A budget that fits in runes but not in bytes must return the full message.
	got := truncateHTMLBody("u", "Priroda i društvo", msgtypes.Grade, descriptions, grade, runes)
	if got != full {
		t.Errorf("message was truncated at a rune budget that fits; the limit must be counted in runes, not bytes\ngot %d runes, want %d",
			utf8.RuneCountInString(got), runes)
	}
}

// TestSlackInitIsIdempotent: the shared client is created lazily and reused, so
// a second call must not replace a live client mid-cycle.
// Not parallel: writes the package-level slackCli global.
func TestSlackInitIsIdempotent(t *testing.T) {
	slackMu.Lock()
	orig := slackCli
	slackCli = nil
	slackMu.Unlock()

	t.Cleanup(func() {
		slackMu.Lock()
		slackCli = orig
		slackMu.Unlock()
	})

	if err := slackInit("xoxb-first"); err != nil {
		t.Fatalf("slackInit() = %v, want nil", err)
	}

	slackMu.Lock()
	first := slackCli
	slackMu.Unlock()

	if first == nil {
		t.Fatal("slackInit() left slackCli nil")
	}

	if err := slackInit("xoxb-second"); err != nil {
		t.Fatalf("slackInit() second call = %v, want nil", err)
	}

	slackMu.Lock()
	second := slackCli
	slackMu.Unlock()

	if first != second {
		t.Error("slackInit() replaced an existing client; initialization must be idempotent")
	}
}

// TestWhatsAppEffectiveUserIDsWithoutGroups: with no group names configured
// there is nothing to resolve, so the configured IDs pass straight through
// without touching the client — which is why a nil client is safe here.
// Not parallel: reads the package-level group-resolution cache.
func TestWhatsAppEffectiveUserIDsWithoutGroups(t *testing.T) {
	userIDs := []string{"111@s.whatsapp.net", "222@s.whatsapp.net"}

	got := whatsAppEffectiveUserIDs(t.Context(), nil, userIDs, nil)
	if !reflect.DeepEqual(got, userIDs) {
		t.Errorf("whatsAppEffectiveUserIDs() = %v, want the configured IDs unchanged", got)
	}

	if got := whatsAppEffectiveUserIDs(t.Context(), nil, nil, nil); len(got) != 0 {
		t.Errorf("whatsAppEffectiveUserIDs() = %v, want empty", got)
	}
}

// TestWhatsAppEffectiveUserIDsServesFreshCache: within the TTL the cached
// resolution is served without a GetJoinedGroups round trip. A nil client
// proves no lookup happened — a regression would nil-panic here rather than
// quietly issuing a network call every send.
// Not parallel: writes the package-level group-resolution cache.
func TestWhatsAppEffectiveUserIDsServesFreshCache(t *testing.T) {
	cached := []string{"111@s.whatsapp.net", "9990001@g.us"}

	whatsAppGroupsMu.Lock()
	origResolved, origAt, origIDs := whatsAppGroupsResolved, whatsAppGroupsResolvedAt, whatsAppResolvedUserIDs
	whatsAppGroupsResolved = true
	whatsAppGroupsResolvedAt = time.Now()
	whatsAppResolvedUserIDs = cached
	whatsAppGroupsMu.Unlock()

	t.Cleanup(func() {
		whatsAppGroupsMu.Lock()
		whatsAppGroupsResolved, whatsAppGroupsResolvedAt, whatsAppResolvedUserIDs = origResolved, origAt, origIDs
		whatsAppGroupsMu.Unlock()
	})

	got := whatsAppEffectiveUserIDs(t.Context(), nil, []string{"111@s.whatsapp.net"}, []string{"Razred 5.a"})
	if !reflect.DeepEqual(got, cached) {
		t.Errorf("whatsAppEffectiveUserIDs() = %v, want the cached resolution %v", got, cached)
	}
}

// TestWhatsAppPersistResolvedGroups covers the config rewrite that turns
// resolved group names into JIDs. Persisting them is what lets later runs skip
// the GetJoinedGroups round trip entirely, so Groups must be cleared as UserIDs
// are filled — leaving both populated would re-resolve forever and append
// duplicate JIDs.
// Not parallel: takes the package-level configRewriteMu.
func TestWhatsAppPersistResolvedGroups(t *testing.T) {
	confFile := filepath.Join(t.TempDir(), ".e-dnevnik.toml")
	if err := os.WriteFile(confFile, []byte(`
[whatsapp]
userids = ["111@s.whatsapp.net"]
groups = ["Razred 5.a"]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	resolved := []string{"111@s.whatsapp.net", "9990001@g.us"}

	whatsAppPersistResolvedGroups(confFile, resolved)

	cfg, err := config.LoadConfigRaw(confFile)
	if err != nil {
		t.Fatalf("LoadConfigRaw() after rewrite failed: %v", err)
	}

	if !reflect.DeepEqual(cfg.WhatsApp.UserIDs, resolved) {
		t.Errorf("persisted UserIDs = %v, want %v", cfg.WhatsApp.UserIDs, resolved)
	}

	if len(cfg.WhatsApp.Groups) != 0 {
		t.Errorf("persisted Groups = %v, want empty — a resolved group must stop being re-resolved, or its JID is appended again every run",
			cfg.WhatsApp.Groups)
	}
}

// TestWhatsAppPersistResolvedGroupsUnwritableConfig: persistence is
// best-effort. A read-only config is a legitimate deployment (a mounted
// secret), and must be logged and skipped rather than crashing the messenger.
// Not parallel: takes the package-level configRewriteMu.
func TestWhatsAppPersistResolvedGroupsUnwritableConfig(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits do not block writes")
	}

	confFile := filepath.Join(t.TempDir(), ".e-dnevnik.toml")

	const original = "[whatsapp]\nuserids = [\"111@s.whatsapp.net\"]\ngroups = [\"Razred 5.a\"]\n"

	if err := os.WriteFile(confFile, []byte(original), 0o400); err != nil {
		t.Fatal(err)
	}

	whatsAppPersistResolvedGroups(confFile, []string{"9990001@g.us"})

	data, err := os.ReadFile(confFile)
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != original {
		t.Error("a read-only configuration was rewritten; the isWriteable guard must skip the rewrite")
	}
}

// TestWhatsAppPersistResolvedGroupsMissingConfig: a config that vanished
// mid-run must be logged and skipped, not panic the send path.
// Not parallel: takes the package-level configRewriteMu.
func TestWhatsAppPersistResolvedGroupsMissingConfig(t *testing.T) {
	whatsAppPersistResolvedGroups(filepath.Join(t.TempDir(), "gone.toml"), []string{"9990001@g.us"})
}
