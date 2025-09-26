// @license
// Copyright (C) 2025  Dinko Korunic
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
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package encdec

import (
	"reflect"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

func TestEncodeDecode(t *testing.T) {
	t.Parallel()
	// Prepare a sample message slice
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

	// Test case 1: Successful encoding and decoding
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

	// Test case 2: Empty slice
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

	// Test case 3: Invalid gob data
	invalidData := []byte("this is not a gob")
	_, err = DecodeMsgs(invalidData)
	if err == nil {
		t.Error("DecodeMsgs should have failed with invalid data, but it did not")
	}
}
