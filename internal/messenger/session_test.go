// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveWhatsAppSessionDeletesStore: callers follow this with "please link
// again", so a surviving store means the next run reuses a dead session.
func TestRemoveWhatsAppSessionDeletesStore(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "wa.sqlite")
	if err := os.WriteFile(path, []byte("session"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := removeWhatsAppSession(path); err != nil {
		t.Fatalf("removeWhatsAppSession() = %v, want nil", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the session store is still present (stat err %v)", err)
	}
}

// TestRemoveWhatsAppSessionAbsentIsSuccess: an event storm races several
// handlers through here, and the losers must not report a problem.
func TestRemoveWhatsAppSessionAbsentIsSuccess(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "never-created.sqlite")

	if err := removeWhatsAppSession(path); err != nil {
		t.Errorf("removeWhatsAppSession() on an absent store = %v, want nil", err)
	}
}

// TestRemoveWhatsAppSessionReportsRealFailure: a discarded failure tells the
// operator to re-link while the stale store stays on disk.
func TestRemoveWhatsAppSessionReportsRealFailure(t *testing.T) {
	t.Parallel()

	// A non-empty directory cannot be removed by os.Remove.
	dir := filepath.Join(t.TempDir(), "not-a-file")
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := removeWhatsAppSession(dir); err == nil {
		t.Error("removeWhatsAppSession() = nil on an undeletable path; the failure must reach the caller")
	}
}
