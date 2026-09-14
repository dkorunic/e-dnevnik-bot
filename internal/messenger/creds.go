// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import "crypto/sha256"

// credGuard records which credentials a cached client was built from, so a
// change forces a rebuild. It keeps a digest, not the credentials: a rotated
// secret should not linger in a package global.
//
// Config is read once at startup, so nothing varies today. The guard exists so
// that stops being load-bearing.
type credGuard struct {
	digest [32]byte
	set    bool
}

// changed reports whether creds differ from those last recorded, treating a
// guard that has never recorded as changed.
func (g *credGuard) changed(creds ...string) bool {
	return !g.set || credDigest(creds) != g.digest
}

// record stores creds' digest, and must run only after a successful build.
func (g *credGuard) record(creds ...string) {
	g.digest = credDigest(creds)
	g.set = true
}

// credSep keeps a field-boundary shift from producing the same digest —
// ("ab","c") must not read as ("a","bc"). internal/sqlitedb separates its own
// hash inputs for the same reason.
const credSep = byte(0x00)

// credDigest hashes the separated credential fields.
func credDigest(creds []string) [32]byte {
	var buf []byte

	for _, c := range creds {
		buf = append(buf, c...)
		buf = append(buf, credSep)
	}

	return sha256.Sum256(buf)
}
