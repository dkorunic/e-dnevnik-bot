// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"fmt"
	"slices"
	"testing"

	ehttp "github.com/enetx/http"
)

// redirectRecorder answers the first request with a 302 so the client follows
// it, and records the headers of every hop.
type redirectRecorder struct {
	hops []*ehttp.Request
}

func (r *redirectRecorder) RoundTrip(req *ehttp.Request) (*ehttp.Response, error) {
	r.hops = append(r.hops, req.Clone(req.Context()))

	if len(r.hops) == 1 {
		h := make(ehttp.Header)
		h.Set("Location", "https://ocjene.skole.hr/class")

		return &ehttp.Response{
			StatusCode: 302,
			Header:     h,
			Body:       ehttp.NoBody,
			Request:    req,
		}, nil
	}

	return stringResponse(req, 200, "<html>logged-in-user</html>"), nil
}

// TestRedirectHopDropsOrigin: a Chrome 152 capture shows no Origin on a GET
// navigation, but the redirect copier carries the POST's onto every hop — a
// value contradicting the profile beneath it, which the contract treats as
// louder than sending nothing.
func TestRedirectHopDropsOrigin(t *testing.T) {
	rec := &redirectRecorder{}

	c := newStubClient(t.Context(), rec)
	c.csrfToken = "tok"

	_ = c.doSAMLRequest()

	if len(rec.hops) < 2 {
		t.Fatalf("recorded %d hop(s); the 302 was not followed", len(rec.hops))
	}

	post, redirected := rec.hops[0], rec.hops[1]

	// The form submit itself is a navigation Chrome does send Origin on.
	if post.Method != ehttp.MethodPost {
		t.Fatalf("hop 1 method = %v, want POST", post.Method)
	}

	if got := post.Header.Get("Origin"); got == "" {
		t.Error("the login POST lost its Origin; Chrome sends one on a form submission")
	}

	if redirected.Method != ehttp.MethodGet {
		t.Fatalf("hop 2 method = %v, want GET", redirected.Method)
	}

	if got := redirected.Header.Get("Origin"); got != "" {
		t.Errorf("the redirect GET carries Origin: %q — Chrome sends none on a GET navigation", got)
	}

	// enetx/http's ordered writer injects Content-Length whenever the order
	// list names it (header.go: `if v == "content-length" && cl >= 0`), so a
	// bodyless GET inheriting the POST's order emits "Content-Length: 0".
	// Chrome sends none on a navigation — confirmed on the wire.
	if slices.Contains(redirected.Header[ehttp.HeaderOrderKey], "content-length") {
		t.Error("the redirect GET still orders content-length, so the writer will inject Content-Length: 0")
	}

	if got := redirected.Header.Get("Content-Length"); got != "" {
		t.Errorf("the redirect GET carries Content-Length: %q", got)
	}

	// Deliberately still present: the same capture shows Chrome sending both
	// on navigations, so stripping them would be its own deviation.
	for _, keep := range []string{"Sec-Fetch-User", "Upgrade-Insecure-Requests"} {
		if redirected.Header.Get(keep) == "" {
			t.Errorf("the redirect GET dropped %v, which Chrome does send on a navigation", keep)
		}
	}
}

// TestRedirectHopUsesChromeGetOrder: two things differ from the POST order the
// hop inherits — body entries go, and the client-hint trio moves after
// sec-fetch-dest. Chrome's fresh-navigation order carries the trio early, so
// "match our plain GETs" is the wrong target; only the capture settles it.
func TestRedirectHopUsesChromeGetOrder(t *testing.T) {
	rec := &redirectRecorder{}

	c := newStubClient(t.Context(), rec)
	c.csrfToken = "tok"

	_ = c.doSAMLRequest()

	if len(rec.hops) < 2 {
		t.Fatalf("recorded %d hop(s)", len(rec.hops))
	}

	order := rec.hops[1].Header[ehttp.HeaderOrderKey]

	// Nothing body-related. cache-control is excluded: the redirect inherits
	// the POST's max-age=0, as Chrome's does.
	for _, gone := range []string{"content-length", "content-type", "pragma", "origin"} {
		if idx := indexOf(order, gone); idx >= 0 {
			t.Errorf("redirect GET still orders %q at %d: %v", gone, idx, order)
		}
	}

	// The trio sits between sec-fetch-dest and referer, as a block.
	dest, ref := indexOf(order, "sec-fetch-dest"), indexOf(order, "referer")
	if dest < 0 || ref < 0 {
		t.Fatalf("order lacks sec-fetch-dest or referer: %v", order)
	}

	for i, hint := range []string{"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform"} {
		at := indexOf(order, hint)
		if at != dest+1+i {
			t.Errorf("%q at %d, want %d (between sec-fetch-dest and referer): %v", hint, at, dest+1+i, order)
		}
	}
}

func indexOf(ss []string, s string) int {
	for i, x := range ss {
		if x == s {
			return i
		}
	}

	return -1
}

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

// TestRedirectCeilingSurvivesOriginStrip: the fixup wraps surf's redirect
// policy, which carries the max-redirect ceiling and same-host rule. Replacing
// it — as the builder's checkRedirect option does — unbounds a redirect loop.
func TestRedirectCeilingSurvivesOriginStrip(t *testing.T) {
	rt := &alwaysRedirect{}

	c := newStubClient(t.Context(), rt)

	_, _ = c.getGeneric("https://ocjene.skole.hr/class")

	if rt.hops == 0 {
		t.Fatal("no request was made")
	}

	// Any ceiling at all; the point is that one exists.
	if rt.hops > 64 {
		t.Errorf("followed %d redirects; surf's ceiling was lost when its policy was wrapped", rt.hops)
	}

	t.Logf("redirect chain stopped after %d hops", rt.hops)
}
