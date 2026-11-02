// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	mail "github.com/wneessen/go-mail"
)

// Port 465 expects TLS from the first byte, while STARTTLS waits for a plaintext
// banner: each side waits on the other until the timeout, on every send. Failing
// on the test server's untrusted certificate proves the client opened with TLS,
// and that it verifies what it finds.
func TestMailImplicitTLSOpensWithHandshake(t *testing.T) {
	t.Parallel()

	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer srv.Close()

	host, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	portInt, _ := strconv.Atoi(port)

	cli, err := newMailClient(host, portInt, true, "u", "p")
	if err != nil {
		t.Fatalf("newMailClient() = %v", err)
	}

	m := mail.NewMsg()
	_ = m.From("from@example.com")
	_ = m.To("to@example.com")
	m.SetBodyString(mail.TypeTextPlain, "x")

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	if err := cli.DialAndSendWithContext(ctx, m); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("DialAndSend error = %v, want a certificate failure from an opening handshake", err)
	}
}
