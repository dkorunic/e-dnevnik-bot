// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/enetx/g"
)

// surf skips certificate checks unless told otherwise, and the fingerprint looks
// the same either way, so nothing else here would notice. Without this anyone on
// the network path could pose as the portal and collect students' passwords.
func TestClientRejectsUntrustedCertificate(t *testing.T) {
	t.Parallel()

	var hits atomic.Int32

	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()

	cli, err := newSurfClient(&Client{ctx: context.Background()})
	if err != nil {
		t.Fatalf("newSurfClient() = %v", err)
	}

	if _, err := cli.Get(g.String(srv.URL)).Do().Result(); err == nil {
		t.Error("request to a server with an untrusted certificate succeeded")
	}

	if n := hits.Load(); n != 0 {
		t.Errorf("server received %d requests, want none", n)
	}
}
