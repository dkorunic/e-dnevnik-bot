// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package sqlitedb

import (
	"crypto/sha256"
	"errors"
	"os"
	"strings"
	"sync"
)

// Oversized buffers are dropped rather than recycled, bounding pool memory.
const hashBufPoolMaxCap = 4 * 1024

var hashBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 256)

		return &b
	},
}

// sqliteURIEscape encodes the characters that break a "file:" DSN — '%', '?'
// and '#' — so a path containing them cannot truncate the filename or corrupt
// the pragma query. SQLite decodes them back; the Replacer encodes each once.
func sqliteURIEscape(path string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
}

// dbExists reports whether filePath exists. Stat, not Lstat, so a dangling
// symlink reads as absent: otherwise first-run seeding is skipped and the next
// run floods.
func dbExists(filePath string) bool {
	_, err := os.Stat(filePath)

	return !errors.Is(err, os.ErrNotExist)
}

// Separates hash inputs so a boundary shift cannot collide: without it
// ["10.","5"] and ["10",".5"] digest identically, and a changed grade reads as a
// duplicate. 0x00 never occurs in the portal's text.
const hashSep = byte(0x00)

// hashContent digests (bucket, subBucket, target) with hashSep separators.
//
// NOTE: older releases wrote a separator-less concatenation. CheckAndFlagTTL
// looks up both so existing installs migrate lazily rather than re-alerting on
// every historical event.
func hashContent(bucket, subBucket string, target []string) []byte {
	return hashParts(bucket, subBucket, target, true)
}

// hashContentLegacy is the pre-separator format, kept only to recognise rows
// flagged by older releases. Never use it for new writes.
func hashContentLegacy(bucket, subBucket string, target []string) []byte {
	return hashParts(bucket, subBucket, target, false)
}

// hashParts implements both digest formats over a pooled scratch buffer.
func hashParts(bucket, subBucket string, target []string, withSep bool) []byte {
	// The extra len(target)+1 covers the separators.
	totalLen := len(bucket) + len(subBucket) + len(target) + 1
	for i := range target {
		totalLen += len(target[i])
	}

	// Grow when the input exceeds capacity.
	bufp := hashBufPool.Get().(*[]byte) //nolint:forcetypeassert // package-private pool; New returns this type

	if cap(*bufp) < totalLen {
		*bufp = make([]byte, 0, totalLen)
	}

	buf := (*bufp)[:0]

	buf = append(buf, bucket...)
	if withSep {
		buf = append(buf, hashSep)
	}

	buf = append(buf, subBucket...)

	for i := range target {
		if withSep {
			buf = append(buf, hashSep)
		}

		buf = append(buf, target[i]...)
	}

	targetHash256 := sha256.Sum256(buf)

	// Bounds steady-state pool memory.
	if cap(buf) <= hashBufPoolMaxCap {
		*bufp = buf
		hashBufPool.Put(bufp)
	}

	return targetHash256[:]
}
