// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

// Package codec serialises and deserialises the persistent failed-message
// queue using CBOR (RFC 8949) via github.com/fxamacker/cbor. Despite the
// historical name "encdec", no encryption is performed: the queue is stored in
// plaintext CBOR inside the local sqlite database. The on-disk database is
// operator-owned, so confidentiality is assumed at the filesystem level rather
// than at the payload level.
package codec

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/fxamacker/cbor/v2"
)

// ErrDecodePanic signals cbor.Decode panicked on malformed input rather than returning cleanly.
var ErrDecodePanic = errors.New("panic while decoding message queue")

// encMode encodes time.Time as an RFC3339 string with nanosecond precision
// (CBOR tag 0) instead of the library default of integer Unix seconds, which
// would silently truncate sub-second precision on msgtypes.Message.Timestamp.
var encMode = mustEncMode()

func mustEncMode() cbor.EncMode {
	em, err := cbor.EncOptions{Time: cbor.TimeRFC3339Nano}.EncMode()
	if err != nil {
		// Options are static, so this can only fail on a programming error.
		panic(err)
	}

	return em
}

// DecodeMsgs takes a byte slice, decodes it as a CBOR-encoded
// slice of msgtypes.Message, and returns the decoded slice and any
// decoding error.
//
// The decoder is hardened against panics here: on-disk queue bytes may come
// from an older binary or have been corrupted on disk. While CBOR decoding
// normally returns malformed input as an error, wrapping Decode in
// defer/recover converts any unexpected panic into a regular error so the
// caller can log and start fresh instead of crashing the whole daemon.
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

// EncodeMsgs encodes a given list of messages using CBOR encoding and returns
// the []byte representation of the messages. If there is an error during encoding,
// the function returns it.
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
