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

// hashBufPoolMaxCap caps recycled buffers; oversized ones are dropped to bound pool memory.
const hashBufPoolMaxCap = 4 * 1024

var hashBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, 256)

		return &b
	},
}

// sqliteURIEscape percent-encodes the characters that break a "file:" SQLite
// DSN — '%' (encoding introducer), '?' (query separator), '#' (fragment) — so a
// path containing them can't truncate the filename or corrupt the pragma query.
// SQLite's URI parser decodes them back. The Replacer encodes each byte once.
func sqliteURIEscape(path string) string {
	return strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
}

// dbExists reports whether filePath exists. os.Stat (not Lstat) so a dangling
// symlink reads as absent — otherwise first-run seeding is skipped and the next
// run floods.
func dbExists(filePath string) bool {
	_, err := os.Stat(filePath)

	return !errors.Is(err, os.ErrNotExist)
}

// hashSep separates hash inputs so boundary shifts between adjacent parts
// cannot collide. target holds scraped portal content (grade fields), so
// without a delimiter e.g. target=["10.","5"] and target=["10",".5"] would
// produce identical digests and a changed grade could be misread as a
// duplicate. 0x00 never occurs in the portal's text content.
const hashSep = byte(0x00)

// hashContent creates a SHA-256 hash from (bucket, subBucket, []target) joined
// with hashSep separators and returns the raw 32-byte digest.
//
// NOTE: rows written by older releases used a separator-less concatenation
// (see hashContentLegacy). CheckAndFlagTTL performs a dual lookup so existing
// installs migrate lazily instead of re-alerting on every historical event.
func hashContent(bucket, subBucket string, target []string) []byte {
	return hashParts(bucket, subBucket, target, true)
}

// hashContentLegacy is the pre-separator digest format, kept only so
// CheckAndFlagTTL can recognise rows flagged by older releases. Do not use
// for new writes.
func hashContentLegacy(bucket, subBucket string, target []string) []byte {
	return hashParts(bucket, subBucket, target, false)
}

// hashParts implements both digest formats over a pooled scratch buffer.
func hashParts(bucket, subBucket string, target []string, withSep bool) []byte {
	// +len(target)+1 covers the worst-case separator count.
	totalLen := len(bucket) + len(subBucket) + len(target) + 1
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

	// Skip re-pooling oversized buffers to bound steady-state pool memory.
	if cap(buf) <= hashBufPoolMaxCap {
		*bufp = buf
		hashBufPool.Put(bufp)
	}

	return targetHash256[:]
}
