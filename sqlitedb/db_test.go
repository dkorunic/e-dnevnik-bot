// @license
// Copyright (C) 2026 Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

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
	// t.TempDir() yields a unique, auto-cleaned directory per test run, so
	// concurrent or repeat invocations cannot race on a shared filename.
	tmpFile := filepath.Join(t.TempDir(), "test-db-for-testing.db.sqlite")

	// Test database creation.
	eDB, err := New(context.Background(), tmpFile)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Test Existing() on a new database.
	// Note: dbExists logic is: checks if file exists BEFORE opening.
	// In New():
	// isExisting := dbExists(filePath)
	// db, err := sql.Open(...)
	// ...
	// So if file didn't exist, isExisting should be false.
	if eDB.Existing() {
		t.Error("Existing() should be false for a new database")
	}

	// Test CheckAndFlagTTL on a new key.
	found, err := eDB.CheckAndFlagTTL(context.Background(), "test-bucket", "test-sub-bucket", []string{"test-target"})
	if err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}
	if found {
		t.Error("CheckAndFlagTTL() should return false for a new key")
	}

	// Test CheckAndFlagTTL on an existing key.
	found, err = eDB.CheckAndFlagTTL(context.Background(), "test-bucket", "test-sub-bucket", []string{"test-target"})
	if err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}
	if !found {
		t.Error("CheckAndFlagTTL() should return true for an existing key")
	}

	// Test FetchAndStore on a new key.
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

	// Test FetchAndStore on an existing key.
	err = eDB.FetchAndStore(context.Background(), key, func(old []byte) ([]byte, error) {
		if !bytes.Equal(old, []byte("new-value")) {
			t.Errorf("unexpected old value: got %v, want %v", old, []byte("new-value"))
		}
		return []byte("updated-value"), nil
	})
	if err != nil {
		t.Fatalf("FetchAndStore() failed: %v", err)
	}

	// Verify the updated value.
	err = eDB.FetchAndStore(context.Background(), key, func(old []byte) ([]byte, error) {
		if !bytes.Equal(old, []byte("updated-value")) {
			t.Errorf("unexpected old value: got %v, want %v", old, []byte("updated-value"))
		}
		return old, nil
	})
	if err != nil {
		t.Fatalf("FetchAndStore() failed: %v", err)
	}

	// Close the database.
	if err := eDB.Close(); err != nil {
		t.Fatalf("Close() failed: %v", err)
	}

	// Test Existing() on an existing database.
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
	// Same inputs should produce the same hash (deterministic).
	h1 := hashContent("bucket", "sub", []string{"a", "b"})
	h2 := hashContent("bucket", "sub", []string{"a", "b"})

	if !bytes.Equal(h1, h2) {
		t.Error("hashContent() is not deterministic")
	}

	// Different inputs should produce different hashes.
	h3 := hashContent("bucket", "sub", []string{"a", "c"})

	if bytes.Equal(h1, h3) {
		t.Error("hashContent() produced the same hash for different inputs")
	}

	// Hash length should be 32 bytes (SHA-256).
	if len(h1) != 32 {
		t.Errorf("hashContent() hash length = %d, want 32", len(h1))
	}
}

func TestDbExists(t *testing.T) {
	t.Parallel()
	// Non-existent path should return false.
	if dbExists("/non/existent/path/that/does/not/exist.db") {
		t.Error("dbExists() returned true for non-existent path")
	}

	// Existing file should return true.
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

	// Insert a key whose TTL has already expired.
	key := hashContent("bucket", "sub", []string{"expired-key"})
	_, err = eDB.db.Exec(
		"INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, ?)",
		key, []byte(""), time.Now().Add(-time.Hour).Unix(),
	)
	if err != nil {
		t.Fatalf("failed to insert expired key: %v", err)
	}

	// Expired key should be treated as not found → return false.
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
	// Insert a key with an already-expired TTL.
	_, err = eDB.db.Exec(
		"INSERT OR REPLACE INTO kv (key, value, expires_at) VALUES (?, ?, ?)",
		key, []byte("old-value"), time.Now().Add(-time.Hour).Unix(),
	)
	if err != nil {
		t.Fatalf("failed to insert expired key: %v", err)
	}

	// FetchAndStore on an expired key should present nil to the transform function.
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

	// Pre-computed oracle: SHA-256("usersubjectfield1").
	// Recompute with: echo -n "usersubjectfield1" | sha256sum
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

	// Bug 6A: swapping bucket and subBucket must produce a DIFFERENT hash.
	swapped := hashContent("subject", "user", []string{"field1"})
	if bytes.Equal(got, swapped) {
		t.Error("hashContent(bucket, subBucket) == hashContent(subBucket, bucket) — swap not detectable")
	}

	// Bug 6B: dropping subBucket from the computation must produce a DIFFERENT hash.
	// The "drop subBucket" mutation would hash "user"+"field1" instead of
	// "user"+"subject"+"field1", changing the digest.
	// We verify this indirectly: a call that excludes "subject" from the input
	// bytes must not equal the golden value.
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

	// First call: key is new → must return false.
	found, err := eDB.CheckAndFlagTTL(context.Background(), "bucket", "sub", []string{"key"})
	if err != nil {
		t.Fatalf("first CheckAndFlagTTL failed: %v", err)
	}
	if found {
		t.Fatal("first CheckAndFlagTTL should return false for a new key")
	}

	// Immediate second call: key now has a future TTL → must return true.
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

	// Insert a key with an already-expired TTL directly.
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
