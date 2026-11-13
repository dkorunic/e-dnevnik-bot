// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"fmt"
	"testing"

	ehttp "github.com/enetx/http"
)

// alwaysRedirect 302s forever, to a fresh path each time.
type alwaysRedirect struct {
	hops int
}

func (a *alwaysRedirect) RoundTrip(req *ehttp.Request) (*ehttp.Response, error) {
	a.hops++

	// Self-limiting so a lost ceiling fails the assertion below rather than
	// wedging the test binary in an unbounded loop.
	if a.hops > 128 {
		return stringResponse(req, 200, "stopped"), nil
	}

	h := make(ehttp.Header)
	h.Set("Location", fmt.Sprintf("https://ocjene.skole.hr/hop%d", a.hops))

	return &ehttp.Response{
		StatusCode: 302,
		Header:     h,
		Body:       ehttp.NoBody,
		Request:    req,
	}, nil
}

// TestRedirectCeilingHolds: surf's redirect policy carries the max-redirect
// ceiling and same-host rule. Replacing it — as the builder's checkRedirect
// option does — unbounds a redirect loop.
func TestRedirectCeilingHolds(t *testing.T) {
	rt := &alwaysRedirect{}

	c := newStubClient(t.Context(), rt)

	_, _ = c.getGeneric("https://ocjene.skole.hr/class")

	if rt.hops == 0 {
		t.Fatal("no request was made")
	}

	// Any ceiling at all; the point is that one exists.
	if rt.hops > 64 {
		t.Errorf("followed %d redirects; surf's ceiling was lost", rt.hops)
	}

	t.Logf("redirect chain stopped after %d hops", rt.hops)
}
