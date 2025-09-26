// @license
// Copyright (C) 2025 Dinko Korunic
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

package db

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestDBOperations(t *testing.T) {
	// Create a temporary directory path for the database, but don't create the directory itself.
	tmpdir := filepath.Join(os.TempDir(), "test-db-for-testing")
	// Clean up any previous test runs.
	os.RemoveAll(tmpdir)
	defer os.RemoveAll(tmpdir)

	// Test database creation.
	eDB, err := New(tmpdir)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	// Test Existing() on a new database.
	if eDB.Existing() {
		t.Error("Existing() should be false for a new database")
	}

	// Test CheckAndFlagTTL on a new key.
	found, err := eDB.CheckAndFlagTTL("test-bucket", "test-sub-bucket", []string{"test-target"})
	if err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}
	if found {
		t.Error("CheckAndFlagTTL() should return false for a new key")
	}

	// Test CheckAndFlagTTL on an existing key.
	found, err = eDB.CheckAndFlagTTL("test-bucket", "test-sub-bucket", []string{"test-target"})
	if err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}
	if !found {
		t.Error("CheckAndFlagTTL() should return true for an existing key")
	}

	// Test FetchAndStore on a new key.
	key := []byte("test-key")
	err = eDB.FetchAndStore(key, func(old []byte) ([]byte, error) {
		if old != nil {
			t.Errorf("old value should be nil for a new key, but got %v", old)
		}
		return []byte("new-value"), nil
	})
	if err != nil {
		t.Fatalf("FetchAndStore() failed: %v", err)
	}

	// Test FetchAndStore on an existing key.
	err = eDB.FetchAndStore(key, func(old []byte) ([]byte, error) {
		if !bytes.Equal(old, []byte("new-value")) {
			t.Errorf("unexpected old value: got %v, want %v", old, []byte("new-value"))
		}
		return []byte("updated-value"), nil
	})
	if err != nil {
		t.Fatalf("FetchAndStore() failed: %v", err)
	}

	// Verify the updated value.
	err = eDB.FetchAndStore(key, func(old []byte) ([]byte, error) {
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
	eDB, err = New(tmpdir)
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
