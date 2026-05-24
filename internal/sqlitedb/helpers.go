// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package sqlitedb

import (
	"errors"
	"os"
	"sync"

	"github.com/minio/sha256-simd"
)

// hashBufPoolMaxCap caps recycled buffers; oversized ones are dropped to bound pool memory.
const hashBufPoolMaxCap = 4 * 1024

var hashBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 256)

		return &b
	},
}

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
	totalLen := len(bucket) + len(subBucket)
	for i := range target {
		totalLen += len(target[i])
	}

	// Pooled buffer; grow when input exceeds capacity.
	bufp := hashBufPool.Get().(*[]byte)

	if cap(*bufp) < totalLen {
		*bufp = make([]byte, 0, totalLen)
	}

	buf := (*bufp)[:0]

	buf = append(buf, bucket...)
	buf = append(buf, subBucket...)

	for i := range target {
		buf = append(buf, target[i]...)
	}

	targetHash256 := sha256.Sum256(buf)

	// Skip re-pooling oversized buffers to bound steady-state pool memory.
	if cap(buf) <= hashBufPoolMaxCap {
		*bufp = buf
		hashBufPool.Put(bufp)
	}

	return targetHash256[:]
}
