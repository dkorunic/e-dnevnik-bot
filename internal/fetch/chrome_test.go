// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"io"
	"strings"
	"testing"

	http "github.com/enetx/http"
)

// captureRequests wires a Client to a stub transport that records every request
// it is handed, so the assertions can inspect the exact wire-bound headers.
func captureRequests(t *testing.T, body string) (*Client, *[]*http.Request) {
	t.Helper()

	seen := make([]*http.Request, 0, 4)

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		seen = append(seen, req)

		return stringResponse(req, http.StatusOK, body), nil
	}))

	return c, &seen
}

const authedBody = `<html><body class="logged-in-user">ok</body></html>`

// TestColdNavigationSendsNoReferer pins the first hop: nothing visited yet, so
// Chrome reports Sec-Fetch-Site: none and omits Referer.
func TestColdNavigationSendsNoReferer(t *testing.T) {
	t.Parallel()

	c, seen := captureRequests(t, authedBody)

	if _, err := c.getGeneric(ClassURL); err != nil {
		t.Fatalf("getGeneric: %v", err)
	}

	req := (*seen)[0]

	if got := req.Header.Get("Sec-Fetch-Site"); got != "none" {
		t.Errorf("Sec-Fetch-Site = %q, want none", got)
	}

	if got := req.Header.Get("Referer"); got != "" {
		t.Errorf("Referer = %q, want none on a cold navigation", got)
	}
}

// TestRefererFollowsNavigationChain proves Referer tracks the page actually
// visited last rather than staying pinned to a constant.
func TestRefererFollowsNavigationChain(t *testing.T) {
	t.Parallel()

	c, seen := captureRequests(t, authedBody)

	if _, err := c.getGeneric(ClassURL); err != nil {
		t.Fatalf("getGeneric(ClassURL): %v", err)
	}

	if _, err := c.getGeneric(GradeAllURL); err != nil {
		t.Fatalf("getGeneric(GradeAllURL): %v", err)
	}

	req := (*seen)[1]

	if got := req.Header.Get("Referer"); got != ClassURL {
		t.Errorf("Referer on second hop = %q, want %q", got, ClassURL)
	}

	if got := req.Header.Get("Sec-Fetch-Site"); got != "same-origin" {
		t.Errorf("Sec-Fetch-Site = %q, want same-origin once a page has been visited", got)
	}
}

// TestLoginPostLooksLikeFormNavigation covers the correction to surf's profile,
// which models POSTs as fetches. A form submission is a navigation, and every
// value it carries must be the profile's own: its Accept is compared with a GET
// from the same profile, never a pinned string that ages with Chrome.
func TestLoginPostLooksLikeFormNavigation(t *testing.T) {
	t.Parallel()

	c, seen := captureRequests(t, authedBody)

	if _, err := c.getGeneric(ClassURL); err != nil {
		t.Fatalf("getGeneric: %v", err)
	}

	c.csrfToken = "tok"

	if err := c.doSAMLRequest(); err != nil {
		t.Fatalf("doSAMLRequest: %v", err)
	}

	get, req := (*seen)[0], (*seen)[1]

	// surf generates same-origin for a fetch and none for a navigation; a raw
	// Header.Set of the former is taken for its own default and replaced.
	want := map[string]string{
		"Accept":                    get.Header.Get("Accept"),
		"Sec-Fetch-Site":            "same-origin",
		"Referer":                   ClassURL,
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
		"Origin":                    BaseURL,
		"Content-Type":              "application/x-www-form-urlencoded",
	}

	for k, v := range want {
		if got := req.Header.Get(k); got != v {
			t.Errorf("header %q = %q, want %q", k, got, v)
		}
	}

	// A form submit is a reload-like navigation, so Chrome sends max-age=0
	// (CLAUDE.md for why a link click does not).
	if got := req.Header.Get("Cache-Control"); got != "max-age=0" {
		t.Errorf("Cache-Control = %q, want \"max-age=0\" on a form submission", got)
	}

	// Chrome sends no Pragma on any hop.
	if got := req.Header.Get("Pragma"); got != "" {
		t.Errorf("Pragma = %q, want it not sent", got)
	}
}

// TestAcceptEncodingAdvertisesModernCodecs guards a value surf's profile
// supplies: Go alone would advertise gzip only.
func TestAcceptEncodingAdvertisesModernCodecs(t *testing.T) {
	t.Parallel()

	c, seen := captureRequests(t, authedBody)

	if _, err := c.getGeneric(GradeAllURL); err != nil {
		t.Fatalf("getGeneric: %v", err)
	}

	if got := (*seen)[0].Header.Get("Accept-Encoding"); got != "gzip, deflate, br, zstd" {
		t.Errorf("Accept-Encoding = %q, want the four codecs Chrome advertises", got)
	}
}

// TestLoginPostCanReplay guards the login against surf's h2-to-HTTP/1.1 retry,
// which only a request with GetBody survives. No local listener negotiates that
// fallback, so the wire tests cannot catch a body surf stops rewinding, and
// bodyless GETs replay regardless.
func TestLoginPostCanReplay(t *testing.T) {
	t.Parallel()

	c, seen := captureRequests(t, authedBody)
	c.csrfToken = "tok"

	if err := c.doSAMLRequest(); err != nil {
		t.Fatalf("doSAMLRequest: %v", err)
	}

	req := (*seen)[0]
	if req.GetBody == nil {
		t.Fatal("login POST has no GetBody; surf cannot replay it over HTTP/1.1")
	}

	for range 2 {
		rc, err := req.GetBody()
		if err != nil {
			t.Fatalf("GetBody: %v", err)
		}

		b, _ := io.ReadAll(rc)
		if !strings.Contains(string(b), "csrf_token=tok") {
			t.Fatalf("replayed body = %q, want the login form", b)
		}
	}
}
