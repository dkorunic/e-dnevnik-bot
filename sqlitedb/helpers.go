// @license
// Copyright (C) 2026  Dinko Korunic
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

package sqlitedb

import (
	"errors"
	"os"

	"github.com/minio/sha256-simd"
)

// dbExists checks if the path exists on the filesystem and returns boolean.
func dbExists(filePath string) bool {
	_, err := os.Lstat(filePath)

	return !errors.Is(err, os.ErrNotExist)
}

// hashContent creates a SHA-256 hash from (bucket, subBucket, []target) concatenated without separators and returns
// the raw 32-byte digest. SHA-256 is collision-resistant in the cryptographic sense (no known practical collisions),
// but callers should be aware that the inputs are joined without delimiters: distinct logical tuples whose string
// representations share the same byte sequence (e.g. bucket="ab",subBucket="c" vs bucket="a",subBucket="bc") will
// produce identical hashes. This is acceptable here because bucket and subBucket are fixed, application-controlled
// values, not arbitrary user input.
func hashContent(bucket, subBucket string, target []string) []byte {
	// get total length of all strings
	totalLen := len(bucket) + len(subBucket)
	for i := range target {
		totalLen += len(target[i])
	}

	// pre-allocate buffer
	buf := make([]byte, 0, totalLen)

	buf = append(buf, bucket...)
	buf = append(buf, subBucket...)

	for i := range target {
		buf = append(buf, target[i]...)
	}

	// calculate SHA-256 using SIMD AVX512 or SHA Extensions where possible
	targetHash256 := sha256.Sum256(buf)

	return targetHash256[:]
}
