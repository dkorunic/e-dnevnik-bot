// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"encoding/binary"
	"net"
	"testing"
	"time"

	"github.com/enetx/g"
)

// captureClientHello points the production client at a throwaway TCP listener
// and returns the raw first flight. The handshake never completes — the
// ClientHello is the whole point.
func captureClientHello(t *testing.T) []byte {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	defer ln.Close()

	ch := make(chan []byte, 1)

	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			ch <- nil

			return
		}

		defer conn.Close()

		buf := make([]byte, 4096)
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _ := conn.Read(buf)
		ch <- buf[:n]
	}()

	c, err := NewClientWithContext(t.Context(), "user@skole.hr", "s3cret")
	if err != nil {
		t.Fatalf("NewClientWithContext: %v", err)
	}

	defer c.CloseConnections()

	// Expected to fail — the listener never speaks TLS back. Backgrounded so the
	// test waits on the ClientHello, not on the dead connection timing out.
	go func() {
		_ = c.httpClient.Get(g.String("https://" + ln.Addr().String() + "/")).Do()
	}()

	select {
	case raw := <-ch:
		if len(raw) == 0 {
			t.Fatal("captured no ClientHello bytes")
		}

		return raw
	case <-time.After(15 * time.Second):
		t.Fatal("timed out waiting for ClientHello")

		return nil
	}
}

// isGREASE reports whether v is one of the reserved GREASE values (0x0a0a,
// 0x1a1a ... 0xfafa) Chrome sprinkles through its ClientHello.
func isGREASE(v uint16) bool {
	return byte(v>>8) == byte(v) && byte(v)&0x0f == 0x0a
}

// clientHelloCipherSuites pulls the cipher-suite list out of a raw ClientHello.
func clientHelloCipherSuites(t *testing.T, raw []byte) []uint16 {
	t.Helper()

	// 5-byte TLS record header, then handshake type (1) + 3-byte length,
	// client_version (2) and random (32).
	const fixed = 5 + 4 + 2 + 32

	if len(raw) < fixed+1 {
		t.Fatalf("ClientHello too short: %d bytes", len(raw))
	}

	if raw[0] != 0x16 || raw[5] != 0x01 {
		t.Fatalf("not a ClientHello handshake record: type=%#x msg=%#x", raw[0], raw[5])
	}

	p := fixed
	p += 1 + int(raw[p]) // session_id

	if len(raw) < p+2 {
		t.Fatal("truncated before cipher suites")
	}

	n := int(binary.BigEndian.Uint16(raw[p:]))
	p += 2

	if len(raw) < p+n {
		t.Fatal("truncated cipher suite list")
	}

	suites := make([]uint16, 0, n/2)
	for i := p; i < p+n; i += 2 {
		suites = append(suites, binary.BigEndian.Uint16(raw[i:]))
	}

	return suites
}

// TestClientHelloCarriesChromeGREASE proves the impersonated ClientHello drives
// the handshake: crypto/tls never emits GREASE, so a GREASE cipher in the first
// flight cannot come from the standard library.
func TestClientHelloCarriesChromeGREASE(t *testing.T) {
	t.Parallel()

	suites := clientHelloCipherSuites(t, captureClientHello(t))

	if len(suites) == 0 {
		t.Fatal("no cipher suites in ClientHello")
	}

	if !isGREASE(suites[0]) {
		t.Errorf("first cipher suite = %#04x, want a GREASE value; crypto/tls would never send one", suites[0])
	}
}
