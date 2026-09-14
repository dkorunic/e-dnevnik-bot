// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"net/http"
	"slices"

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

	order, ordered := h[ehttp.HeaderOrderKey]
	if ordered {
		order = append([]string{"connection"}, order...)
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

		// surf's POST order map lists neither (its GET map does), and unlisted
		// keys sort after every listed one — both would land past Cookie.
		if ordered {
			order = insertHeaderOrder(order, "upgrade-insecure-requests", "user-agent")
			order = insertHeaderOrder(order, "sec-fetch-user", "sec-fetch-dest")
		}
	}

	if ordered {
		h[ehttp.HeaderOrderKey] = order
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

// postOnlyHeaderOrder belong to a request with a body. A hop rewritten to GET
// inherits them because the redirect copier strips only Content-Type.
var postOnlyHeaderOrder = []string{"content-length", "content-type", "pragma", "cache-control", "origin"}

// chromeRedirectClientHints move as a block: a Chrome 152 capture puts them
// after sec-fetch-dest on a redirect hop, where a fresh navigation carries them
// ahead of user-agent. Matching our own plain GETs is therefore also wrong.
var chromeRedirectClientHints = []string{"sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform"}

// moveHeaderOrderBefore relocates names, in the sequence given, to sit ahead of
// before. A missing before leaves order untouched rather than guessed at.
func moveHeaderOrderBefore(order, names []string, before string) []string {
	out := slices.Clone(order)

	present := make([]string, 0, len(names))

	for _, n := range names {
		if slices.Contains(order, n) {
			present = append(present, n)
		}

		out = dropHeaderOrder(out, n)
	}

	at := slices.Index(out, before)
	if at < 0 {
		return order
	}

	return slices.Insert(out, at, present...)
}

// dropHeaderOrder removes name from order, leaving it untouched if absent.
func dropHeaderOrder(order []string, name string) []string {
	at := slices.Index(order, name)
	if at < 0 {
		return order
	}

	return slices.Delete(slices.Clone(order), at, at+1)
}

// insertHeaderOrder slots name ahead of before, leaving order untouched if
// name is already listed or before is absent; appending instead would recreate
// the trailing-header bug this prevents.
func insertHeaderOrder(order []string, name, before string) []string {
	if slices.Contains(order, name) {
		return order
	}

	at := slices.Index(order, before)
	if at < 0 {
		return order
	}

	return slices.Insert(slices.Clone(order), at, name)
}

// fixupRedirectHeaders corrects a hop rewritten to GET that still wears the
// POST's headers. chromeNavigationMW runs once per Do(), not per hop, so
// CheckRedirect is the only hook that sees one — and it runs after the copier.
// Every correction comes from a Chrome 152 wire capture (see CLAUDE.md).
//
//   - Origin goes: Chrome sends none on a GET navigation.
//   - content-length goes from the *order list*, not the header map. Deleting
//     the header does nothing — enetx/http's ordered writer re-adds it from the
//     list (`if v == "content-length" && cl >= 0`), emitting "Content-Length: 0".
//   - the client hints move to where the capture shows them.
//
// Sec-Fetch-User and Upgrade-Insecure-Requests stay: Chrome sends both on
// navigations. A 307/308 keeps its method, so it keeps its Origin too.
//
// The order is patched, not declared: surf owns it via Impersonate()'s own
// SetHeaders call, and passing our own MapOrd would register at priority 0 —
// racing that call rather than following it — and replace the list wholesale.
// Wrapping surf's CheckRedirect likewise preserves its max-redirect ceiling and
// same-host rule, which the builder's checkRedirect option discards.
func fixupRedirectHeaders(cli *surf.Client) {
	hc := cli.GetClient()
	surfPolicy := hc.CheckRedirect

	hc.CheckRedirect = func(req *ehttp.Request, via []*ehttp.Request) error {
		if req.Method == http.MethodGet {
			req.Header.Del("Origin")

			if order, ok := req.Header[ehttp.HeaderOrderKey]; ok {
				for _, name := range postOnlyHeaderOrder {
					order = dropHeaderOrder(order, name)
				}

				req.Header[ehttp.HeaderOrderKey] = moveHeaderOrderBefore(order, chromeRedirectClientHints, "referer")
			}
		}

		if surfPolicy != nil {
			return surfPolicy(req, via)
		}

		return nil
	}
}

// newSurfClient builds c's impersonating HTTP client. One Chrome 152 profile
// supplies every browser-identifying signal — user agent, client hints, header
// order, ClientHello, HTTP/2 SETTINGS — so they cannot contradict each other.
//
// Session() adds the cookie jar SSO needs; owning the client keeps
// CloseConnections off a shared pool.
func newSurfClient(c *Client) (*surf.Client, error) {
	cli, err := surf.NewClient().
		Builder().
		Impersonate().Windows().Chrome().
		Session().
		Timeout(Timeout).
		With(c.chromeNavigationMW, overrideMWPriority).
		Build().
		Result()
	if err != nil {
		return nil, err
	}

	fixupRedirectHeaders(cli)

	return cli, nil
}
