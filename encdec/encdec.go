// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package encdec

import (
	"bytes"
	"encoding/gob"
	"errors"
	"fmt"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

// ErrDecodePanic is returned when gob.Decode panics (malformed/corrupt input
// that trips an unchecked internal invariant rather than a clean error return).
var ErrDecodePanic = errors.New("panic while decoding message queue")

// DecodeMsgs takes a byte slice, decodes it as a GOB-encoded
// slice of msgtypes.Message, and returns the decoded slice and any
// decoding error.
//
// The gob decoder is hardened against panics here: on-disk queue bytes may
// come from an older binary or have been corrupted on disk, and a few
// historical gob paths panic on malformed type metadata rather than returning
// an error. Wrapping Decode in defer/recover converts any such panic into a
// regular error so the caller can log and start fresh instead of crashing the
// whole daemon.
func DecodeMsgs(val []byte) (msgs []msgtypes.Message, err error) {
	if len(val) == 0 {
		return []msgtypes.Message{}, nil
	}

	defer func() {
		if r := recover(); r != nil {
			msgs = nil
			err = fmt.Errorf("%w: %v", ErrDecodePanic, r)
		}
	}()

	buf := bytes.NewBuffer(val)
	dec := gob.NewDecoder(buf)

	err = dec.Decode(&msgs)

	return msgs, err
}

// EncodeMsgs encodes a given list of messages using GOB encoding and returns
// the []byte representation of the messages. If there is an error during encoding,
// the function returns it.
func EncodeMsgs(msgs []msgtypes.Message) ([]byte, error) {
	if len(msgs) == 0 {
		return []byte{}, nil
	}

	buf := bytes.NewBuffer(make([]byte, 0, len(msgs)*512))
	enc := gob.NewEncoder(buf)

	err := enc.Encode(msgs)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
