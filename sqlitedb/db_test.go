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

	// Oracle: SHA-256("usersubjectfield1"). Recompute via `echo -n ... | sha256sum`.
	expected := []byte{
		0x35, 0xb8, 0x03, 0xf7, 0x3d, 0x4f, 0xe3, 0xbc,
		0xb9, 0xfc, 0xcd, 0xf1, 0x75, 0x50, 0xe2, 0x34,
		0x3d, 0x5e, 0x74, 0xc0, 0x55, 0xab, 0x79, 0x9c,
		0x11, 0x7f, 0xab, 0x3c, 0x92, 0x61, 0x08, 0x22,
	}

	got := hashContent("user", "subject", []string{"field1"})

	if !bytes.Equal(got, expected) {
		t.Errorf("hashContent order mismatch:\ngot:  %x\nwant: %x\n(bucket/subBucket swapped or subBucket dropped?)", got, expected)
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
