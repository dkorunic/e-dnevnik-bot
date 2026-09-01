// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package codec

import (
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// FuzzDecodeMsgs pins the contract that makes the failed-message queue
// survivable: DecodeMsgs must return an error for any input, never panic.
//
// Queue rows are read back from disk on every startup, so a decoder panic on a
// corrupted row is not a one-off failure — it is an unrecoverable boot loop,
// with the daemon crashing on the same row every time it starts. That is why
// DecodeMsgs carries an explicit recover.
//
// The current cbor/v2 rejects every malformed input tested here cleanly, so the
// recover is presently unreachable insurance rather than a live code path. This
// fuzz target is what would notice if a library upgrade changed that: it asserts
// the property (no panic escapes) rather than the mechanism.
func FuzzDecodeMsgs(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte{})
	f.Add([]byte{0xff, 0xfe, 0xfd})
	f.Add([]byte{0x82, 0x01})
	f.Add([]byte{0x9b, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0xc2, 0x49, 0x01, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0x7f, 0x61, 0x61, 0xff})

	// A well-formed encoding, so the fuzzer has a valid shape to mutate from.
	if valid, err := EncodeMsgs([]msgtypes.Message{{
		Timestamp:    time.Unix(1700000000, 123456789).UTC(),
		Username:     "pero.peric@skole.hr",
		Subject:      "Matematika",
		Descriptions: []string{"Ocjena"},
		Fields:       []string{"5"},
		Code:         msgtypes.Grade,
	}}); err == nil {
		f.Add(valid)
	}

	f.Fuzz(func(_ *testing.T, data []byte) {
		// Panic-freedom is the entire assertion: a panic escaping DecodeMsgs
		// fails the test. The return values are deliberately not constrained,
		// because two legitimate shapes would otherwise look like violations
		// and neither harms any caller:
		//
		//   0x81 0x30 — cbor fills the destination slice before failing on the
		//     element, returning one zero-valued Message *and* an error. Every
		//     caller discards the messages when err != nil.
		//   0xf6      — CBOR null decodes to a nil slice with a nil error.
		//     Callers treat an empty result as "nothing queued" and drop the row.
		_, _ = DecodeMsgs(data)
	})
}
