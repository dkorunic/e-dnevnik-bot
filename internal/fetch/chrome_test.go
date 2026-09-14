// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"bytes"
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
// which models POSTs as XHR. A form submission is a navigation.
func TestLoginPostLooksLikeFormNavigation(t *testing.T) {
	t.Parallel()

	c, seen := captureRequests(t, authedBody)
	c.csrfToken = "tok"

	if err := c.doSAMLRequest(); err != nil {
		t.Fatalf("doSAMLRequest: %v", err)
	}

	req := (*seen)[0]

	want := map[string]string{
		"Accept":                    chromeNavAccept,
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

	// Pragma is surf's XHR profile leaking; Chrome sends none on any hop.
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

// wireHeaderOrder serialises req the way it goes onto the connection and
// returns the header names in order. Asserting on the wire bytes rather than on
// the order pseudo-header tests the transport, not our configuration.
func wireHeaderOrder(t *testing.T, req *http.Request) []string {
	t.Helper()

	var buf bytes.Buffer
	if err := req.Write(&buf); err != nil {
		t.Fatalf("req.Write: %v", err)
	}

	var names []string

	for line := range strings.SplitSeq(buf.String(), "\r\n") {
		if strings.HasPrefix(line, "GET ") || strings.HasPrefix(line, "POST ") {
			continue
		}

		if name, _, found := strings.Cut(line, ":"); found && name != "" {
			names = append(names, name)
		}
	}

	return names
}

// TestHeaderOrderMatchesChrome guards the ordering surf's profile supplies. Go
// emits headers alphabetically, which no browser does.
func TestHeaderOrderMatchesChrome(t *testing.T) {
	t.Parallel()

	c, seen := captureRequests(t, authedBody)
	c.lastURL = ClassURL

	if _, err := c.getGeneric(GradeAllURL); err != nil {
		t.Fatalf("getGeneric: %v", err)
	}

	got := wireHeaderOrder(t, (*seen)[0])

	want := []string{
		"Sec-Ch-Ua",
		"Sec-Ch-Ua-Mobile",
		"Sec-Ch-Ua-Platform",
		"Upgrade-Insecure-Requests",
		"User-Agent",
		"Accept",
		"Sec-Fetch-Site",
		"Sec-Fetch-Mode",
		"Sec-Fetch-User",
		"Sec-Fetch-Dest",
		"Referer",
		"Accept-Encoding",
		"Accept-Language",
	}

	idx := 0

	for _, name := range got {
		if idx < len(want) && strings.EqualFold(name, want[idx]) {
			idx++
		}
	}

	if idx != len(want) {
		t.Errorf("header order mismatch: matched %d/%d\n got: %v\nwant order: %v", idx, len(want), got, want)
	}
}

// TestNoPriorityHeaderOverHTTP1 pins a deviation found by diffing against a real
// Chrome 152 navigation: Priority is HTTP/2-only, surf inserts it regardless, and
// this portal negotiates no ALPN.
func TestNoPriorityHeaderOverHTTP1(t *testing.T) {
	t.Parallel()

	c, seen := captureRequests(t, authedBody)

	if _, err := c.getGeneric(GradeAllURL); err != nil {
		t.Fatalf("getGeneric: %v", err)
	}

	if got := (*seen)[0].Header.Get("Priority"); got != "" {
		t.Errorf("GET Priority = %q, want it absent — real Chrome sends none on HTTP/1.1", got)
	}

	// Same connection, same rule.
	pc, pseen := captureRequests(t, authedBody)
	pc.csrfToken = "tok"

	if err := pc.doSAMLRequest(); err != nil {
		t.Fatalf("doSAMLRequest: %v", err)
	}

	if got := (*pseen)[0].Header.Get("Priority"); got != "" {
		t.Errorf("POST Priority = %q, want it absent — real Chrome sends none on HTTP/1.1", got)
	}
}

// TestConnectionHeaderSitsAfterHost pins the same diff's other finding: Chrome
// sends Connection directly after Host, and surf's order map has no slot for it.
func TestConnectionHeaderSitsAfterHost(t *testing.T) {
	t.Parallel()

	c, seen := captureRequests(t, authedBody)

	if _, err := c.getGeneric(GradeAllURL); err != nil {
		t.Fatalf("getGeneric: %v", err)
	}

	if got := (*seen)[0].Header.Get("Connection"); got != "keep-alive" {
		t.Fatalf("Connection = %q, want keep-alive", got)
	}

	order := wireHeaderOrder(t, (*seen)[0])

	if len(order) < 2 || !strings.EqualFold(order[0], "Host") || !strings.EqualFold(order[1], "Connection") {
		t.Errorf("wire order starts %v, want Host then Connection", order[:min(2, len(order))])
	}
}

// TestPostHeaderOrderMatchesChrome guards the login POST against the capture in
// chromePostWireOrder.
//
// It once asserted surf's own output under a name claiming it matched Chrome,
// which is why that deviation survived until a capture was taken. One shared
// expectation now, so this and the socket-level test cannot drift apart.
func TestPostHeaderOrderMatchesChrome(t *testing.T) {
	t.Parallel()

	c, seen := captureRequests(t, authedBody)
	c.csrfToken = "tok"
	c.lastURL = LoginURL

	if err := c.doSAMLRequest(); err != nil {
		t.Fatalf("doSAMLRequest: %v", err)
	}

	assertWireOrder(t, "login POST", wireHeaderOrder(t, (*seen)[0]), chromePostWireOrder)
}
