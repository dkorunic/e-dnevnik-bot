// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"net/http"

	ehttp "github.com/enetx/http"
	"github.com/enetx/surf"
)

// chromeNavAccept is Chrome's top-level navigation Accept. surf already uses it
// for GET; it is named here because the login POST must be corrected back to it.
const chromeNavAccept = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp," +
	"image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7"

// overrideMWPriority outranks surf's header pipeline, which registers at 0.
// surf runs middleware lowest-priority-first, so a smaller value here would
// discard every override below without erroring.
const overrideMWPriority = 999

// chromeNavigationMW corrects what surf's Chrome profile cannot know about this
// portal, and nothing more: everything identifying the browser stays the
// profile's, so those signals cannot drift apart. See the browser impersonation
// contract in CLAUDE.md.
func (c *Client) chromeNavigationMW(r *surf.Request) error {
	req := r.GetRequest()
	h := req.Header

	h.Set("Accept-Language", AcceptLanguageHR)

	// HTTP/2-only header that surf inserts unconditionally; this portal negotiates
	// no ALPN, so real Chrome could not send it here. Deleted before the POST
	// block so it stays gone on both paths. Revisit if the portal enables h2.
	h.Del("Priority")

	// Chrome sends this on HTTP/1.1 and places it directly after Host; surf's
	// order map has no slot for it, so it would otherwise land last.
	h.Set("Connection", "keep-alive")

	if order, ok := h[ehttp.HeaderOrderKey]; ok {
		h[ehttp.HeaderOrderKey] = append([]string{"connection"}, order...)
	}

	// surf models POSTs as XHR; this one is a form submission, which Chrome sends
	// as a navigation.
	if req.Method == http.MethodPost {
		h.Set("Content-Type", "application/x-www-form-urlencoded")
		h.Set("Accept", chromeNavAccept)
		h.Set("Sec-Fetch-Mode", "navigate")
		h.Set("Sec-Fetch-Dest", "document")
		h.Set("Sec-Fetch-User", "?1")
		h.Set("Upgrade-Insecure-Requests", "1")

		// Chrome attaches Origin to form submissions, same-origin included.
		h.Set("Origin", BaseURL)

		// Chrome pairs these only on a hard reload.
		h.Del("Cache-Control")
		h.Del("Pragma")
	}

	// A cold navigation has no predecessor, so no Referer.
	if c.lastURL == "" {
		h.Set("Sec-Fetch-Site", "none")
		h.Del("Referer")
	} else {
		h.Set("Sec-Fetch-Site", "same-origin")
		h.Set("Referer", c.lastURL)
	}

	return nil
}

// newSurfClient builds c's impersonating HTTP client. One Chrome 152 profile
// supplies every browser-identifying signal — user agent, client hints, header
// order, ClientHello, HTTP/2 SETTINGS — so they cannot contradict each other.
//
// Session() adds the cookie jar SSO needs; owning the client keeps
// CloseConnections off a shared pool.
func newSurfClient(c *Client) (*surf.Client, error) {
	return surf.NewClient().
		Builder().
		Impersonate().Windows().Chrome().
		Session().
		Timeout(Timeout).
		With(c.chromeNavigationMW, overrideMWPriority).
		Build().
		Result()
}
