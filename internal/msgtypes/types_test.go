// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package msgtypes_test

import (
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// TestEventCodeOrdinalsAreStable pins the enum's numeric values. EventCode is
// CBOR-encoded into the on-disk failed-message queue, so these ordinals are a
// persisted wire format, not an implementation detail: inserting or reordering
// a constant would make messages queued by an older build decode as the wrong
// event type after an upgrade — a queued Exam silently becoming a Reading.
// New codes must therefore be appended, never inserted.
func TestEventCodeOrdinalsAreStable(t *testing.T) {
	t.Parallel()

	want := map[msgtypes.EventCode]int{
		msgtypes.Grade:        0,
		msgtypes.Exam:         1,
		msgtypes.Reading:      2,
		msgtypes.FinalGrade:   3,
		msgtypes.NationalExam: 4,
	}

	for code, ordinal := range want {
		if int(code) != ordinal {
			t.Errorf("EventCode %d has ordinal %d, want %d — appending is safe, reordering corrupts the persisted queue",
				ordinal, int(code), ordinal)
		}
	}
}

// TestMessageZeroValueIsUsable checks the zero value needs no construction: the
// pipeline builds Messages as struct literals with only some fields set, and a
// zero Timestamp is a meaningful "no date from the portal" signal that the
// relevance filter treats as fail-open.
func TestMessageZeroValueIsUsable(t *testing.T) {
	t.Parallel()

	var m msgtypes.Message

	if m.Code != msgtypes.Grade {
		t.Errorf("zero Code = %v, want Grade (the iota-zero constant)", m.Code)
	}

	if !m.Timestamp.IsZero() {
		t.Error("zero Timestamp should be the zero time — the relevance filter keys off IsZero")
	}

	if !m.QueuedAt.IsZero() {
		t.Error("zero QueuedAt should be the zero time — MaxQueueAge keys off IsZero for legacy rows")
	}

	if m.Fields != nil || m.Descriptions != nil || m.SkipRecipients != nil {
		t.Error("zero-value slices should be nil so callers can append without initialising")
	}
}

// TestMessageSliceFieldsAreIndependent guards against a shared backing array
// between messages: SkipRecipients is appended to per messenger during retry,
// and aliasing would leak one messenger's delivery state into another's.
func TestMessageSliceFieldsAreIndependent(t *testing.T) {
	t.Parallel()

	base := msgtypes.Message{
		Timestamp: time.Now(),
		Username:  "u",
		Subject:   "s",
		Fields:    []string{"1.9.", "5"},
	}

	copied := base
	copied.SkipRecipients = append(copied.SkipRecipients, "123")

	if len(base.SkipRecipients) != 0 {
		t.Errorf("appending to a copy mutated the original: %v", base.SkipRecipients)
	}
}
