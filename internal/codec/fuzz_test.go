// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package codec

import (
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
)

// FuzzDecodeMsgs: queue rows are re-read from disk on every startup, so a
// decoder panic on a corrupted row is a boot loop rather than a one-off. Hence
// the recover in DecodeMsgs, and hence this target — which asserts the property
// (no panic escapes) rather than the mechanism.
//
// cbor/v2 currently rejects every malformed input cleanly, so that recover is
// unreachable insurance. This is what would notice if an upgrade changed it.
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
