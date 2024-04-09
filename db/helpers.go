// @license
// Copyright (C) 2022  Dinko Korunic
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

package db

import (
	"bytes"
	"errors"
	"os"

	"github.com/minio/sha256-simd"
)

// dbExists checks if the path exists on the filesystem and returns boolean.
func dbExists(filePath string) bool {
	_, err := os.Lstat(filePath)

	return !errors.Is(err, os.ErrNotExist)
}

// hashContent creates SHA-256 hash from (bucket, subBucket, []target) concatenated strings and returns []byte result.
func hashContent(bucket, subBucket string, target []string) []byte {
	// get total length of all strings
	totalLen := len(bucket) + len(subBucket)
	for i := range target {
		totalLen += len(target[i])
	}

	var sb bytes.Buffer

	// pre-allocate buffer
	sb.Grow(totalLen)

	sb.WriteString(bucket)
	sb.WriteString(subBucket)
	for i := range target {
		sb.WriteString(target[i])
	}

	// calculate SHA-256 using SIMD AVX512 or SHA Extensions where possible
	targetHash256 := sha256.Sum256(sb.Bytes())

	return targetHash256[:]
}
