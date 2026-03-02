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
	// Create a temporary file path for the database
	tmpFile := filepath.Join(os.TempDir(), "test-db-for-testing.db.sqlite")
	// Clean up any previous test runs.
	os.Remove(tmpFile)
	defer os.Remove(tmpFile)

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
	tmpFile := filepath.Join(os.TempDir(), "test-db-ttl-expired.db.sqlite")
	os.Remove(tmpFile)
	defer os.Remove(tmpFile)

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
	tmpFile := filepath.Join(os.TempDir(), "test-db-fetchstore-expired.db.sqlite")
	os.Remove(tmpFile)
	defer os.Remove(tmpFile)

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

func TestCleanupRemovesExpiredKeys(t *testing.T) {
	t.Parallel()
	tmpFile := filepath.Join(os.TempDir(), "test-db-cleanup.db.sqlite")
	os.Remove(tmpFile)
	defer os.Remove(tmpFile)

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
