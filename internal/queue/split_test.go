// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// scriptedStore fails the Put after putsBeforeFail have succeeded, recording
// every key written and deleted. See rowStore.
type scriptedStore struct {
	putsBeforeFail int
	puts           [][]byte
	deletes        [][]byte
}

func (s *scriptedStore) Put(_ context.Context, key, _ []byte) error {
	if len(s.puts) >= s.putsBeforeFail {
		return errors.New("put failed")
	}

	s.puts = append(s.puts, bytes.Clone(key))

	return nil
}

func (s *scriptedStore) Delete(_ context.Context, key []byte) error {
	s.deletes = append(s.deletes, bytes.Clone(key))

	return nil
}

func (s *scriptedStore) deleted(key []byte) bool {
	for _, d := range s.deletes {
		if bytes.Equal(d, key) {
			return true
		}
	}

	return false
}

func threeMessages() []msgtypes.Message {
	return []msgtypes.Message{
		{Code: msgtypes.Exam, Username: "u", Subject: "one"},
		{Code: msgtypes.Exam, Username: "u", Subject: "two"},
		{Code: msgtypes.Exam, Username: "u", Subject: "three"},
	}
}

// TestSplitRowRollsBackPartialSplit is what the layout's crash-safety rests on:
// a Put failing partway must leave no orphans and not touch the original, or
// the next fetch duplicates every row it already wrote.
func TestSplitRowRollsBackPartialSplit(t *testing.T) {
	t.Parallel()

	queueKey := []byte("test-queue")
	origKey := rowKey(queueKey)

	store := &scriptedStore{putsBeforeFail: 2}

	got := splitRow(context.Background(), store, queueKey, origKey, threeMessages())
	if got != nil {
		t.Fatalf("splitRow() = %v, want nil so the caller skips the row this cycle", got)
	}

	// Both rows written before the failure must be gone again.
	for _, k := range store.puts {
		if !store.deleted(k) {
			t.Errorf("row %x was written and not rolled back; the next fetch would return it twice", k)
		}
	}

	// The original is the only remaining copy — deleting it would lose the lot.
	if store.deleted(origKey) {
		t.Error("splitRow deleted the original row after failing to split it; those messages are now lost")
	}
}

// TestSplitRowSucceedsAndRetiresOriginal: one key per survivor and the
// aggregate gone is what makes the caller's per-message Dequeue crash-safe.
func TestSplitRowSucceedsAndRetiresOriginal(t *testing.T) {
	t.Parallel()

	queueKey := []byte("test-queue")
	origKey := rowKey(queueKey)

	store := &scriptedStore{putsBeforeFail: 99}

	survivors := threeMessages()

	got := splitRow(context.Background(), store, queueKey, origKey, survivors)
	if len(got) != len(survivors) {
		t.Fatalf("splitRow() returned %d keys, want %d", len(got), len(survivors))
	}

	// Distinct keys: a shared one would let the first Dequeue orphan the rest.
	for i := range got {
		for j := i + 1; j < len(got); j++ {
			if bytes.Equal(got[i], got[j]) {
				t.Errorf("keys %d and %d are identical (%x)", i, j, got[i])
			}
		}
	}

	if !store.deleted(origKey) {
		t.Error("the original aggregate row survived a successful split; its messages would be delivered twice")
	}

	for _, k := range got {
		if store.deleted(k) {
			t.Errorf("new row %x was deleted during a successful split", k)
		}
	}
}
