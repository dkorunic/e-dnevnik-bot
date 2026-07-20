// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package sqlitedb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDBOperations(t *testing.T) {
	t.Parallel()

	tmpFile := filepath.Join(t.TempDir(), "test-db-for-testing.db.sqlite")

	eDB, err := New(context.Background(), tmpFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	if eDB.Existing() {
		t.Error("Existing() should be false for a new database")
	}

	found, err := eDB.CheckAndFlagTTL(context.Background(), "test-bucket", "test-sub-bucket", []string{"test-target"})
	if err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}
	if found {
		t.Error("CheckAndFlagTTL() should return false for a new key")
	}

	found, err = eDB.CheckAndFlagTTL(context.Background(), "test-bucket", "test-sub-bucket", []string{"test-target"})
	if err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}
	if !found {
		t.Error("CheckAndFlagTTL() should return true for an existing key")
	}

	key := []byte("test-key")
	err = eDB.FetchAndStore(context.Background(), key, func(old []byte) ([]byte, error) {
		if old != nil {
			t.Errorf("old value should be nil for a new key, but got %v", old)
		}
		return []byte("new-value"), nil
	})
	if err != nil {
		t.Fatalf("FetchAndStore() failed: %v", err)
	}

	err = eDB.FetchAndStore(context.Background(), key, func(old []byte) ([]byte, error) {
		if !bytes.Equal(old, []byte("new-value")) {
			t.Errorf("unexpected old value: got %v, want %v", old, []byte("new-value"))
		}
		return []byte("updated-value"), nil
	})
	if err != nil {
		t.Fatalf("FetchAndStore() failed: %v", err)
	}

	err = eDB.FetchAndStore(context.Background(), key, func(old []byte) ([]byte, error) {
		if !bytes.Equal(old, []byte("updated-value")) {
			t.Errorf("unexpected old value: got %v, want %v", old, []byte("updated-value"))
		}
		return old, nil
	})
	if err != nil {
		t.Fatalf("FetchAndStore() failed: %v", err)
	}

	if err := eDB.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	eDB, err = New(context.Background(), tmpFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	if !eDB.Existing() {
		t.Error("Existing() should be true for an existing database")
	}
	if err := eDB.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}
}

func TestHashContent(t *testing.T) {
	t.Parallel()

	h1 := hashContent("bucket", "sub", []string{"a", "b"})
	h2 := hashContent("bucket", "sub", []string{"a", "b"})

	if !bytes.Equal(h1, h2) {
		t.Error("hashContent() is not deterministic")
	}

	h3 := hashContent("bucket", "sub", []string{"a", "c"})

	if bytes.Equal(h1, h3) {
		t.Error("hashContent() produced the same hash for different inputs")
	}

	if len(h1) != 32 {
		t.Errorf("hashContent() hash length = %d, want 32", len(h1))
	}
}

func TestDbExists(t *testing.T) {
	t.Parallel()

	if dbExists("/non/existent/path/that/does/not/exist.db") {
		t.Error("dbExists() returned true for non-existent path")
	}

	tmpfile, err := os.CreateTemp("", "test-db-exists-*.db")
	if err != nil {
		t.Fatal(err)
	}

	tmpfile.Close()
	defer os.Remove(tmpfile.Name())

	if !dbExists(tmpfile.Name()) {
		t.Error("dbExists() returned false for existing file")
	}
}

// TestDbExistsDanglingSymlink verifies dbExists uses os.Stat (follows the link)
// rather than os.Lstat: a dangling symlink at the DB path must read as
// non-existent, otherwise first-run seeding is skipped and the next run floods.
func TestDbExistsDanglingSymlink(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	link := filepath.Join(dir, "dangling.db")
	target := filepath.Join(dir, "missing-target.db")

	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink on this platform: %v", err)
	}

	if dbExists(link) {
		t.Error("dbExists() returned true for a dangling symlink (would suppress first-run seeding)")
	}
}

// TestNewDBPathWithQuestionMark verifies a DB path containing '?' — which would
// corrupt a naively concatenated "file:...?pragma" DSN — is opened correctly:
// the file lands at the literal path and data round-trips across reopen.
func TestNewDBPathWithQuestionMark(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "weird?name.sqlite")

	eDB, err := New(ctx, path)
	if err != nil {
		t.Fatalf("New() with '?' in path failed: %v", err)
	}

	if _, err := eDB.CheckAndFlagTTL(ctx, "b", "s", []string{"t"}); err != nil {
		t.Fatalf("CheckAndFlagTTL failed: %v", err)
	}

	if err := eDB.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	// File at the literal path proves the DSN wasn't truncated at the '?'.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file not created at literal path %q: %v", path, err)
	}

	eDB2, err := New(ctx, path)
	if err != nil {
		t.Fatalf("reopen failed: %v", err)
	}
	defer eDB2.Close() //nolint:errcheck

	found, err := eDB2.CheckAndFlagTTL(ctx, "b", "s", []string{"t"})
	if err != nil {
		t.Fatalf("CheckAndFlagTTL after reopen failed: %v", err)
	}

	if !found {
		t.Error("flagged key not found after reopen — DSN likely targeted the wrong file")
	}
}

func TestCheckAndFlagTTLExpiredKey(t *testing.T) {
	t.Parallel()
	tmpFile := filepath.Join(t.TempDir(), "test-db-ttl-expired.db.sqlite")

	eDB, err := New(context.Background(), tmpFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer eDB.Close()

	key := hashContent("bucket", "sub", []string{"expired-key"})
	_, err = eDB.db.Exec(
		"INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, ?)",
		key, []byte(""), time.Now().Add(-time.Hour).Unix(),
	)
	if err != nil {
		t.Fatalf("failed to insert expired key: %v", err)
	}

	// Expired keys must be treated as not found.
	found, err := eDB.CheckAndFlagTTL(context.Background(), "bucket", "sub", []string{"expired-key"})
	if err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}

	if found {
		t.Error("CheckAndFlagTTL() should return false for an expired key")
	}
}

func TestFetchAndStoreExpiredKey(t *testing.T) {
	t.Parallel()
	tmpFile := filepath.Join(t.TempDir(), "test-db-fetchstore-expired.db.sqlite")

	eDB, err := New(context.Background(), tmpFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer eDB.Close()

	key := []byte("expired-fetch-key")

	_, err = eDB.db.Exec(
		"INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, ?)",
		key, []byte("old-value"), time.Now().Add(-time.Hour).Unix(),
	)
	if err != nil {
		t.Fatalf("failed to insert expired key: %v", err)
	}

	// Expired keys must surface as nil to the transform.
	calledWithNil := false

	err = eDB.FetchAndStore(context.Background(), key, func(old []byte) ([]byte, error) {
		if old == nil {
			calledWithNil = true
		}

		return []byte("new-value"), nil
	})
	if err != nil {
		t.Fatalf("FetchAndStore() failed: %v", err)
	}

	if !calledWithNil {
		t.Error("FetchAndStore() should call the function with nil for an expired key")
	}
}

// TestHashContentConcatenationOrder verifies Bugs 6A and 6B from TESTING-PLAN:
// hashContent must concatenate bucket THEN subBucket THEN target fields.
// Swapping bucket/subBucket (6A) or dropping subBucket (6B) changes the hash.
// The expected value is the SHA-256 of the literal bytes "user"+"subject"+"field1"
// in that order, acting as a golden-value oracle that fails for any reordering.
func TestHashContentConcatenationOrder(t *testing.T) {
	t.Parallel()

	// Oracle: SHA-256("usersubjectfield1") — the LEGACY separator-less format.
	// Recompute via `echo -n ... | sha256sum`. This must stay stable forever:
	// CheckAndFlagTTL relies on it to recognise rows flagged by old releases.
	legacyExpected := []byte{
		0x35, 0xb8, 0x03, 0xf7, 0x3d, 0x4f, 0xe3, 0xbc,
		0xb9, 0xfc, 0xcd, 0xf1, 0x75, 0x50, 0xe2, 0x34,
		0x3d, 0x5e, 0x74, 0xc0, 0x55, 0xab, 0x79, 0x9c,
		0x11, 0x7f, 0xab, 0x3c, 0x92, 0x61, 0x08, 0x22,
	}

	legacy := hashContentLegacy("user", "subject", []string{"field1"})
	if !bytes.Equal(legacy, legacyExpected) {
		t.Errorf("hashContentLegacy drifted from historical format:\ngot:  %x\nwant: %x", legacy, legacyExpected)
	}

	// The current format is separator-delimited and must differ from legacy.
	got := hashContent("user", "subject", []string{"field1"})
	if bytes.Equal(got, legacy) {
		t.Error("hashContent equals hashContentLegacy — separators are not being applied")
	}

	// Bug 6A: swapped bucket/subBucket must change the hash.
	swapped := hashContent("subject", "user", []string{"field1"})
	if bytes.Equal(got, swapped) {
		t.Error("hashContent(bucket, subBucket) == hashContent(subBucket, bucket) — swap not detectable")
	}

	// Bug 6B: omitting subBucket from the input must change the hash.
	droppedSub := hashContent("user", "", []string{"field1"})
	if bytes.Equal(got, droppedSub) {
		t.Error("hashContent without subBucket produced same hash — subBucket is not included in hash input")
	}
}

// TestHashContentBoundaryShift verifies the separator prevents adjacent-part
// boundary collisions: without it, target=["ab","c"] and target=["a","bc"]
// would hash identically and a changed grade could be misread as a duplicate.
func TestHashContentBoundaryShift(t *testing.T) {
	t.Parallel()

	h1 := hashContent("user", "subject", []string{"ab", "c"})
	h2 := hashContent("user", "subject", []string{"a", "bc"})

	if bytes.Equal(h1, h2) {
		t.Error("hashContent boundary-shift collision: [ab c] == [a bc]")
	}

	// Bucket/subBucket boundary must also be protected.
	h3 := hashContent("userx", "subject", nil)
	h4 := hashContent("user", "xsubject", nil)

	if bytes.Equal(h3, h4) {
		t.Error("hashContent bucket/subBucket boundary-shift collision")
	}
}

// TestCheckAndFlagTTLLegacyMigration verifies that an event flagged by an old
// release (separator-less hash key) is still recognised as already-seen after
// the hash format change — the dual lookup prevents a re-alert flood on
// upgrade — and that it gets re-flagged under the current-format key.
func TestCheckAndFlagTTLLegacyMigration(t *testing.T) {
	t.Parallel()

	tmpFile := filepath.Join(t.TempDir(), "test-db-legacy-hash.db.sqlite")

	eDB, err := New(context.Background(), tmpFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer eDB.Close()

	// Simulate a row written by a pre-separator release.
	legacyKey := hashContentLegacy("user", "subject", []string{"field1"})
	_, err = eDB.db.Exec(
		"INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, ?)",
		legacyKey, []byte(""), time.Now().Add(time.Hour).Unix(),
	)
	if err != nil {
		t.Fatalf("failed to insert legacy row: %v", err)
	}

	// Must be treated as already-flagged, not as a brand-new event.
	found, err := eDB.CheckAndFlagTTL(context.Background(), "user", "subject", []string{"field1"})
	if err != nil {
		t.Fatalf("CheckAndFlagTTL failed: %v", err)
	}

	if !found {
		t.Error("legacy-flagged event reported as new — upgrade would re-alert on all historical events")
	}

	// The event must now also exist under the current-format key.
	var count int
	if err = eDB.db.QueryRow("SELECT COUNT(*) FROM kv WHERE key = ?",
		hashContent("user", "subject", []string{"field1"})).Scan(&count); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if count != 1 {
		t.Error("legacy hit was not re-flagged under the current-format key")
	}
}

// TestCheckAndFlagTTLReturnsTrueImmediately verifies Bug 7A from TESTING-PLAN:
// a key that was just flagged must be found on the very next call.
// The inverted-comparison mutation (`<` → `>`) would make every valid key appear
// expired, causing CheckAndFlagTTL to always return false.
func TestCheckAndFlagTTLReturnsTrueImmediately(t *testing.T) {
	t.Parallel()

	tmpFile := filepath.Join(t.TempDir(), "test-db-immediate-recheck.db.sqlite")

	eDB, err := New(context.Background(), tmpFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer eDB.Close()

	found, err := eDB.CheckAndFlagTTL(context.Background(), "bucket", "sub", []string{"key"})
	if err != nil {
		t.Fatalf("first CheckAndFlagTTL failed: %v", err)
	}
	if found {
		t.Fatal("first CheckAndFlagTTL should return false for a new key")
	}

	// Re-check with a future TTL must return true.
	found, err = eDB.CheckAndFlagTTL(context.Background(), "bucket", "sub", []string{"key"})
	if err != nil {
		t.Fatalf("second CheckAndFlagTTL failed: %v", err)
	}
	if !found {
		t.Error("second CheckAndFlagTTL should return true; inverted comparison bug would make it return false")
	}
}

func TestCleanupRemovesExpiredKeys(t *testing.T) {
	t.Parallel()
	tmpFile := filepath.Join(t.TempDir(), "test-db-cleanup.db.sqlite")

	eDB, err := New(context.Background(), tmpFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	defer eDB.Close()

	expiredKey := []byte("cleanup-expired-key")
	_, err = eDB.db.Exec(
		"INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, ?)",
		expiredKey, []byte(""), time.Now().Add(-time.Hour).Unix(),
	)
	if err != nil {
		t.Fatalf("failed to insert expired key: %v", err)
	}

	eDB.cleanup(context.Background())

	var count int
	if err = eDB.db.QueryRow("SELECT COUNT(*) FROM kv WHERE key = ?", expiredKey).Scan(&count); err != nil {
		t.Fatalf("query failed: %v", err)
	}

	if count != 0 {
		t.Error("cleanup() should have removed the expired key")
	}
}
