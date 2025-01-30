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

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

// DecodeMsgs takes a byte slice, decodes it as a GOB-encoded
// slice of msgtypes.Message, and returns the decoded slice and any
// decoding error.
func DecodeMsgs(val []byte) ([]msgtypes.Message, error) {
	buf := bytes.NewBuffer(val)
	dec := gob.NewDecoder(buf)

	var msgs []msgtypes.Message

	// GOB decode []byte to Message list
	err := dec.Decode(&msgs)

	return msgs, err
}

// EncodeMsgs encodes a given list of messages using GOB encoding and returns
// the []byte representation of the messages. If there is an error during encoding,
// the function logs the error and returns it.
func EncodeMsgs(msgs []msgtypes.Message) ([]byte, error) {
	buf := bytes.NewBuffer(nil)
	enc := gob.NewEncoder(buf)

	// GOB encode Message list to []byte
	err := enc.Encode(msgs)

	return buf.Bytes(), err
}
