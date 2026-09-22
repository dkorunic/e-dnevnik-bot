// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

// Package codec serialises the persistent failed-message queue as CBOR
// (RFC 8949).
//
// Despite the historical name "encdec", nothing is encrypted: the queue is
// plaintext CBOR in the local sqlite database, which is operator-owned, so
// confidentiality rests on the filesystem rather than the payload.
package codec

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/fxamacker/cbor/v2"
)

// ErrDecodePanic reports that cbor.Decode panicked rather than returned.
var ErrDecodePanic = errors.New("panic while decoding message queue")

// Encodes time.Time as RFC3339 with nanoseconds rather than the library default
// of integer Unix seconds, which would truncate Message.Timestamp.
var encMode = mustEncMode()

func mustEncMode() cbor.EncMode { //nolint:ireturn // cbor exposes no concrete EncMode
	em, err := cbor.EncOptions{Time: cbor.TimeRFC3339Nano}.EncMode()
	if err != nil {
		// Static options: only a programming error can fail here.
		panic(err)
	}

	return em
}

// DecodeMsgs decodes val into a message slice, recovering a panic on corrupted
// or older-format bytes into ErrDecodePanic so one bad entry cannot crash the
// daemon.
//
//nolint:nonamedreturns // the recover below assigns both returns
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
	dec := cbor.NewDecoder(buf)

	err = dec.Decode(&msgs)

	return msgs, err
}

// EncodeMsgs encodes msgs as CBOR.
func EncodeMsgs(msgs []msgtypes.Message) ([]byte, error) {
	if len(msgs) == 0 {
		return []byte{}, nil
	}

	buf := bytes.NewBuffer(make([]byte, 0, len(msgs)*512))
	enc := encMode.NewEncoder(buf)

	err := enc.Encode(msgs)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
