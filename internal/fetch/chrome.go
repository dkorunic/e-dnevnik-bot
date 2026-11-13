// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"net/http"

	"github.com/enetx/surf"
)

// chromeNavigationMW corrects what surf's Chrome profile cannot know about this
// portal, and nothing more. Every browser-identifying value and the whole
// order — Accept, Fetch Metadata, Cache-Control, Connection, Priority — stay
// the profile's, laid out per transport attempt and per negotiated protocol.
// See the browser impersonation contract in CLAUDE.md.
//
// Values go through SetHeaders, never Header.Set: surf drops a raw value equal
// to one it generated and regenerates it, so Sec-Fetch-Site: same-origin on the
// login POST (generated for a fetch, then regenerated as "none" once it is a
// navigation) would be lost.
func (c *Client) chromeNavigationMW(r *surf.Request) error {
	req := r.GetRequest()

	set := map[string]string{"Accept-Language": AcceptLanguageHR}

	// surf models a POST as a fetch; this one is a form submission, which
	// Chrome sends as a navigation. The profile then supplies the navigation
	// values and order for the POST and for the GET its 302 becomes, and drops
	// Origin again on that GET.
	if req.Method == http.MethodPost {
		r.Navigation()

		// Chrome attaches Origin to form submissions, same-origin included.
		set["Origin"] = BaseURL
		set["Content-Type"] = "application/x-www-form-urlencoded"
	}

	// A cold navigation has no predecessor, so no Referer.
	if c.lastURL == "" {
		set["Sec-Fetch-Site"] = "none"

		req.Header.Del("Referer")
	} else {
		set["Sec-Fetch-Site"] = "same-origin"
		set["Referer"] = c.lastURL
	}

	r.SetHeaders(set)

	return nil
}

// newSurfClient builds c's impersonating HTTP client. One Chrome profile
// supplies every browser-identifying signal — user agent, client hints, header
// order, ClientHello, HTTP/2 SETTINGS — so they cannot contradict each other.
//
// Session() adds the cookie jar SSO needs; owning the client keeps
// CloseConnections off a shared pool. SecureTLS() because surf skips certificate
// checks by default, and this client carries students' passwords. The ClientHello
// is the same either way, so the fingerprint does not depend on it.
func newSurfClient(c *Client) (*surf.Client, error) {
	return surf.NewClient().
		Builder().
		Impersonate().Windows().Chrome().
		Session().
		SecureTLS().
		Timeout(Timeout).
		With(c.chromeNavigationMW).
		Build().
		Result()
}
