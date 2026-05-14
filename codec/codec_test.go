// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package codec

import (
	"reflect"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

func TestEncodeDecode(t *testing.T) {
	t.Parallel()

	originalMsgs := []msgtypes.Message{
		{
			Timestamp:    time.Now().Round(0),
			Username:     "testuser",
			Subject:      "Test Subject 1",
			Descriptions: []string{"Desc 1", "Desc 2"},
			Fields:       []string{"Field 1", "Field 2"},
			Code:         msgtypes.Grade,
		},
		{
			Timestamp:    time.Now().Add(time.Hour).Round(0),
			Username:     "testuser2",
			Subject:      "Test Subject 2",
			Descriptions: []string{"Desc 3"},
			Fields:       []string{"Field 3"},
			Code:         msgtypes.Exam,
		},
	}

	// Round-trip.
	encoded, err := EncodeMsgs(originalMsgs)
	if err != nil {
		t.Fatalf("EncodeMsgs failed: %v", err)
	}

	decoded, err := DecodeMsgs(encoded)
	if err != nil {
		t.Fatalf("DecodeMsgs failed: %v", err)
	}

	if !reflect.DeepEqual(originalMsgs, decoded) {
		t.Errorf("Decoded messages do not match original messages.\nOriginal: %+v\nDecoded:  %+v", originalMsgs, decoded)
	}

	// Empty slice.
	emptyMsgs := []msgtypes.Message{}
	encodedEmpty, err := EncodeMsgs(emptyMsgs)
	if err != nil {
		t.Fatalf("EncodeMsgs with empty slice failed: %v", err)
	}

	decodedEmpty, err := DecodeMsgs(encodedEmpty)
	if err != nil {
		t.Fatalf("DecodeMsgs with empty slice failed: %v", err)
	}

	if len(decodedEmpty) != 0 {
		t.Errorf("Expected empty slice, but got %d messages", len(decodedEmpty))
	}

	// Invalid gob data.
	invalidData := []byte("this is not a gob")
	_, err = DecodeMsgs(invalidData)
	if err == nil {
		t.Error("DecodeMsgs should have failed with invalid data, but it did not")
	}

	// Nil input → empty slice, no error.
	nilResult, err := DecodeMsgs(nil)
	if err != nil {
		t.Fatalf("DecodeMsgs(nil) returned unexpected error: %v", err)
	}

	if len(nilResult) != 0 {
		t.Errorf("DecodeMsgs(nil) should return empty slice, got %d messages", len(nilResult))
	}
}

// TestEncodeDecodEmptyRoundTrip verifies Bug 14 from TESTING-PLAN:
// EncodeMsgs([]Message{}) returns a non-nil []byte{} (not nil).
// DecodeMsgs must treat that non-nil empty slice as "no messages" and return
// an empty list without error. A mutation changing `len(val)==0` to `val==nil`
// in DecodeMsgs would cause gob.NewDecoder to receive zero bytes and return EOF.
func TestEncodeDecodEmptyRoundTrip(t *testing.T) {
	t.Parallel()

	encoded, err := EncodeMsgs([]msgtypes.Message{})
	if err != nil {
		t.Fatalf("EncodeMsgs(empty) failed: %v", err)
	}

	// Must be non-nil so a nil-check in DecodeMsgs can't bypass the len==0 guard.
	if encoded == nil {
		t.Error("EncodeMsgs(empty) returned nil; want non-nil []byte{}")
	}

	decoded, err := DecodeMsgs(encoded)
	if err != nil {
		t.Fatalf("DecodeMsgs(EncodeMsgs(empty)) failed: %v (len==0 guard replaced with val==nil?)", err)
	}

	if len(decoded) != 0 {
		t.Errorf("DecodeMsgs(EncodeMsgs(empty)) returned %d messages, want 0", len(decoded))
	}
}
