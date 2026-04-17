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

	// GOB decode []byte to Message list
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

	// GOB encode Message list to []byte
	err := enc.Encode(msgs)
	if err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}
