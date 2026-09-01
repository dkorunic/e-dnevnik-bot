// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package sqlitedb

import (
	"bytes"
	"path/filepath"
	"testing"
)

// newTestDB opens a throwaway database in a per-test temp dir.
func newTestDB(t *testing.T, name string) *Edb {
	t.Helper()

	eDB, err := New(t.Context(), filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	return eDB
}

// getValue reads key without side effects. The package exposes no Get, so the
// read goes through FetchAndStore with an identity function — which short-
// circuits before the write when the value is unchanged.
func getValue(t *testing.T, eDB *Edb, key []byte) []byte {
	t.Helper()

	var out []byte

	err := eDB.FetchAndStore(t.Context(), key, func(old []byte) ([]byte, error) {
		out = bytes.Clone(old)

		return old, nil
	})
	if err != nil {
		t.Fatalf("FetchAndStore() read failed: %v", err)
	}

	return out
}

// TestPutGetDelete covers the no-TTL KV path used by the queue: Put must be
// readable, overwrite in place, and Delete must be idempotent.
func TestPutGetDelete(t *testing.T) {
	t.Parallel()

	eDB := newTestDB(t, "kv.db.sqlite")
	ctx := t.Context()

	key := []byte("queue\x00msg1")

	if err := eDB.Put(ctx, key, []byte("first")); err != nil {
		t.Fatalf("Put() failed: %v", err)
	}

	if got := getValue(t, eDB, key); !bytes.Equal(got, []byte("first")) {
		t.Errorf("value after Put = %q, want %q", got, "first")
	}

	// INSERT OR REPLACE: a second Put on the same key must overwrite, not error
	// on the primary-key conflict and not leave the old value behind.
	if err := eDB.Put(ctx, key, []byte("second")); err != nil {
		t.Fatalf("Put() overwrite failed: %v", err)
	}

	if got := getValue(t, eDB, key); !bytes.Equal(got, []byte("second")) {
		t.Errorf("value after overwrite = %q, want %q", got, "second")
	}

	if err := eDB.Delete(ctx, key); err != nil {
		t.Fatalf("Delete() failed: %v", err)
	}

	if got := getValue(t, eDB, key); got != nil {
		t.Errorf("value after Delete = %q, want nil", got)
	}

	// Documented contract: deleting an absent key is a no-op, not an error.
	if err := eDB.Delete(ctx, key); err != nil {
		t.Errorf("Delete() of absent key returned %v, want nil", err)
	}
}

// TestPutSurvivesCleanup pins the reason Put writes expires_at NULL: queue rows
// must never be reaped by the TTL cleanup pass. If Put ever grows a TTL, queued
// messages would silently vanish between cycles.
func TestPutSurvivesCleanup(t *testing.T) {
	t.Parallel()

	eDB := newTestDB(t, "kv-cleanup.db.sqlite")
	ctx := t.Context()

	key := []byte("queue\x00persistent")
	if err := eDB.Put(ctx, key, []byte("payload")); err != nil {
		t.Fatalf("Put() failed: %v", err)
	}

	eDB.cleanup(ctx)

	if got := getValue(t, eDB, key); !bytes.Equal(got, []byte("payload")) {
		t.Errorf("value after cleanup = %q, want the row to survive (expires_at must be NULL)", got)
	}
}

// TestScanPrefix checks prefix isolation and ordering: only keys under the
// requested prefix come back, and they come back sorted ascending. A broken
// upper bound would leak neighbouring prefixes into a queue fetch.
func TestScanPrefix(t *testing.T) {
	t.Parallel()

	eDB := newTestDB(t, "kv-scan.db.sqlite")
	ctx := t.Context()

	// "discord\x00" and "discor\x00" are deliberately confusable prefixes.
	seed := map[string]string{
		"discord\x00a": "1",
		"discord\x00c": "3",
		"discord\x00b": "2",
		"discorz\x00a": "no",
		"slack\x00a":   "no",
		"discor\x00a":  "no",
	}
	for k, v := range seed {
		if err := eDB.Put(ctx, []byte(k), []byte(v)); err != nil {
			t.Fatalf("Put(%q) failed: %v", k, err)
		}
	}

	rows, err := eDB.ScanPrefix(ctx, []byte("discord\x00"))
	if err != nil {
		t.Fatalf("ScanPrefix() failed: %v", err)
	}

	wantKeys := []string{"discord\x00a", "discord\x00b", "discord\x00c"}
	if len(rows) != len(wantKeys) {
		t.Fatalf("ScanPrefix() returned %d rows, want %d: %+v", len(rows), len(wantKeys), rows)
	}

	for i, want := range wantKeys {
		if string(rows[i].Key) != want {
			t.Errorf("row %d key = %q, want %q (results must be key-ascending)", i, rows[i].Key, want)
		}
	}

	if string(rows[0].Value) != "1" {
		t.Errorf("row 0 value = %q, want %q", rows[0].Value, "1")
	}
}

// TestScanPrefixEmpty verifies an unmatched prefix yields no rows and no error,
// so callers can range over the result without a nil check.
func TestScanPrefixEmpty(t *testing.T) {
	t.Parallel()

	eDB := newTestDB(t, "kv-scan-empty.db.sqlite")

	rows, err := eDB.ScanPrefix(t.Context(), []byte("nothing-here\x00"))
	if err != nil {
		t.Fatalf("ScanPrefix() failed: %v", err)
	}

	if len(rows) != 0 {
		t.Errorf("ScanPrefix() = %+v, want no rows", rows)
	}
}

// TestScanPrefixAllFFFallback drives the prefixUpperBound == nil branch: an
// all-0xFF prefix has no representable upper bound, so ScanPrefix must fall
// back to an ordered full scan filtered client-side rather than return nothing.
func TestScanPrefixAllFFFallback(t *testing.T) {
	t.Parallel()

	eDB := newTestDB(t, "kv-scan-ff.db.sqlite")
	ctx := t.Context()

	match := []byte{0xFF, 0xFF, 0x01}
	// Sorts after the 0xFF prefix but does not carry it — the client-side
	// bytes.HasPrefix filter is the only thing excluding it in this branch.
	other := []byte{0xFF, 0xFE, 0x01}

	for _, k := range [][]byte{match, other} {
		if err := eDB.Put(ctx, k, []byte("v")); err != nil {
			t.Fatalf("Put(%x) failed: %v", k, err)
		}
	}

	rows, err := eDB.ScanPrefix(ctx, []byte{0xFF, 0xFF})
	if err != nil {
		t.Fatalf("ScanPrefix() failed: %v", err)
	}

	if len(rows) != 1 || !bytes.Equal(rows[0].Key, match) {
		t.Fatalf("ScanPrefix() = %+v, want exactly the 0xFFFF-prefixed key %x", rows, match)
	}
}

// TestPrefixUpperBound pins the byte arithmetic directly: the returned bound
// must be strictly greater than every key carrying the prefix, and trailing
// 0xFF bytes must be truncated rather than wrapped to 0x00.
func TestPrefixUpperBound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		prefix []byte
		want   []byte
	}{
		{
			name:   "simple increment of last byte",
			prefix: []byte("ab"),
			want:   []byte("ac"),
		},
		{
			name:   "queue separator prefix",
			prefix: []byte("discord\x00"),
			want:   []byte("discord\x01"),
		},
		{
			name:   "trailing 0xFF is dropped and carry applied",
			prefix: []byte{0x01, 0xFF},
			want:   []byte{0x02},
		},
		{
			name:   "run of trailing 0xFF collapses to single carry",
			prefix: []byte{0x01, 0xFF, 0xFF, 0xFF},
			want:   []byte{0x02},
		},
		{
			name:   "all 0xFF has no upper bound",
			prefix: []byte{0xFF, 0xFF},
			want:   nil,
		},
		{
			name:   "empty prefix has no upper bound",
			prefix: []byte{},
			want:   nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := prefixUpperBound(tt.prefix)
			if !bytes.Equal(got, tt.want) {
				t.Fatalf("prefixUpperBound(%x) = %x, want %x", tt.prefix, got, tt.want)
			}

			if got == nil {
				return
			}

			// The bound must actually bound: strictly greater than the prefix,
			// which in byte order means greater than every key extending it.
			if bytes.Compare(got, tt.prefix) <= 0 {
				t.Errorf("prefixUpperBound(%x) = %x, which does not sort after the prefix", tt.prefix, got)
			}
		})
	}
}

// TestPrefixUpperBoundDoesNotMutateInput guards the bytes.Clone: ScanPrefix
// passes the caller's queue-name slice, and an in-place increment would corrupt
// the shared package-level queue name for every later call.
func TestPrefixUpperBoundDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	prefix := []byte("discord\x00")
	orig := bytes.Clone(prefix)

	_ = prefixUpperBound(prefix)

	if !bytes.Equal(prefix, orig) {
		t.Errorf("prefixUpperBound mutated its input: %x, want %x", prefix, orig)
	}
}
