// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
	"github.com/enetx/g"
	"github.com/enetx/surf"
	"github.com/jordic/goics"
)

const (
	BaseURL        = "https://ocjene.skole.hr"
	LoginURL       = "https://ocjene.skole.hr/login"
	LoginPath      = "/login" // compared against post-redirect URLs
	ClassURL       = "https://ocjene.skole.hr/class"
	ClassActionURL = "https://ocjene.skole.hr/class_action/%v/course"
	GradeAllURL    = "https://ocjene.skole.hr/grade/all"
	CalendarURL    = "https://ocjene.skole.hr/exam/ical"
	CourseURL      = "https://ocjene.skole.hr/course"

	MaxBodySize = 32 * 1024 * 1024 // 32 MiB upper bound for any single response

	// AcceptLanguageHR forces Croatian responses; the login alert matcher and subject names depend on it.
	AcceptLanguageHR = "hr,hr-HR;q=0.9,en;q=0.1"
)

var (
	ErrUnexpectedStatus  = errors.New("unexpected status code")
	ErrCSRFToken         = errors.New("could not find CSRF token")
	ErrNilBody           = errors.New("client body is nil")
	ErrInvalidLogin      = errors.New("unable to login")
	ErrAuthMarkerMissing = errors.New("login page carried no error and no auth marker")
	ErrSessionExpired    = errors.New("portal session expired")
	ErrBodyTooLarge      = errors.New("response body exceeds size limit")
	ErrInvalidClassID    = errors.New("invalid class ID — refusing to construct URL")
	ErrInvalidHost       = errors.New("portal href resolves to non-portal host — refusing to fetch")

	selCsrfToken  = cascadia.MustCompile(`form > input[name="csrf_token"]`)
	selLoginAlert = cascadia.MustCompile("#page-wrapper > div.flash-messages > div.alert > p")

	// reClassID rejects non-URL-safe class IDs to block path-injection via a tampered portal.
	reClassID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// loggedInMarker sits on the user chrome of every authenticated HTML page; the
// login page never carries it.
var loggedInMarker = []byte("logged-in-user")

// bouncedToLogin reports whether a request ended at the login page — how a
// lapsed session surfaces, since callers would otherwise read a 200 of login
// HTML as a quiet school day.
//
// Data endpoints only: the login POST targets LoginPath either way.
func bouncedToLogin(resp *surf.Response) bool {
	return resp.URL != nil && resp.URL.Path == LoginPath
}

// hasAuthMarker catches a login page served in place of content rather than
// redirected to, which bouncedToLogin cannot see.
//
// Substring test, not a parse: it can only err toward "authenticated", the safe
// direction, since a false "expired" would trigger a re-login storm. Non-HTML is
// exempt because the ICS calendar carries no markers, and a lapsed session
// redirects even that to HTML.
func hasAuthMarker(resp *surf.Response, body []byte) bool {
	if !strings.Contains(resp.Headers.Get("Content-Type").Std(), "text/html") {
		return true
	}

	return bytes.Contains(body, loggedInMarker)
}

// do issues req with the session's context. Headers are chromeNavigationMW's job.
func (c *Client) do(req *surf.Request) (*surf.Response, error) {
	resp, err := req.
		WithContext(c.ctx).
		Do().
		Result()
	if err != nil {
		select {
		case <-c.ctx.Done():
			return nil, c.ctx.Err()
		default:
			return nil, err
		}
	}

	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("%w", ErrNilBody)
	}

	return resp, nil
}

// readBody returns the response body, capped at MaxBodySize.
//
// The limit sits one byte high because surf's Limit truncates silently: reading
// MaxBodySize+1 is what separates a body at the ceiling from one that overran it.
func readBody(resp *surf.Response) ([]byte, error) {
	body, err := resp.Body.Limit(MaxBodySize + 1).Bytes().Result()
	if err != nil {
		return nil, err
	}

	if len(body) > MaxBodySize {
		return nil, fmt.Errorf("%w: %v", ErrBodyTooLarge, resp.URL)
	}

	return body, nil
}

// rememberURL records where the response actually landed, so the next request's
// Referer reflects redirects the way a browser's would.
func (c *Client) rememberURL(resp *surf.Response) {
	if resp.URL != nil {
		c.lastURL = resp.URL.String()
	}
}

// getCSRFToken extracts CSRF Token value hidden in the input form, optionally also getting initial value of cnOcjene
// security cookie.
func (c *Client) getCSRFToken() error {
	resp, err := c.do(c.httpClient.Get(g.String(LoginURL)))
	if err != nil {
		return err
	}

	// surf closes a body only as a side effect of reading it, so paths returning
	// before readBody strand the connection. Close drains and is once-guarded.
	defer resp.Body.Close()

	if int(resp.StatusCode) != http.StatusOK {
		return fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	body, err := readBody(resp)
	if err != nil {
		return err
	}

	c.rememberURL(resp)

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return err
	}

	csrfToken, csrfTokenExists := doc.FindMatcher(selCsrfToken).First().Attr("value")
	if !csrfTokenExists {
		return fmt.Errorf("%w", ErrCSRFToken)
	}

	c.csrfToken = csrfToken

	return nil
}

// doSAMLRequest goes through SSO/SAML authentication, getting SimpleSAMLSessionID SSO cookie and refreshing cnOcjene
// security cookie set in getCSRFToken() step.
func (c *Client) doSAMLRequest() error {
	form := url.Values{
		"username":   {c.username},
		"password":   {c.password},
		"csrf_token": {c.csrfToken},
	}.Encode()

	req := c.httpClient.Post(g.String(LoginURL)).Body(form)

	// surf sets Body but not GetBody, leaving the request unable to replay. The
	// Chrome ClientHello offers h2 while this portal negotiates no ALPN, so surf
	// retries over HTTP/1.1 — possible only for a request it can rewind. Bodyless
	// GETs replay regardless, so without this the login POST alone fails, as an
	// opaque retry loop.
	req.GetRequest().GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(form)), nil
	}

	resp, err := c.do(req)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if int(resp.StatusCode) >= http.StatusBadRequest {
		return fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	body, err := readBody(resp)
	if err != nil {
		return err
	}

	c.rememberURL(resp)

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return err
	}

	// Portal surfaces login failures as a flash-message alert div.
	alertSel := doc.FindMatcher(selLoginAlert)
	if alertSel.Length() > 0 {
		return fmt.Errorf("%w: %v", ErrInvalidLogin, alertSel.Text())
	}

	// A rejected login 302s back to /login rather than 4xx-ing, so the alert
	// markup above is the only guard that can name a cause. Reaching here is
	// ambiguous: drifted alert markup with bad credentials, or a drifted marker
	// on a successful login.
	//
	// Its own sentinel, not ErrInvalidLogin — both are permanent, but reporting
	// a marker rename as "unable to login" points at the wrong problem.
	if !hasAuthMarker(resp, body) {
		return fmt.Errorf("%w: credentials may be wrong, or %q may have been renamed",
			ErrAuthMarkerMissing, loggedInMarker)
	}

	return nil
}

// getGeneric GETs dest and returns the body, capped at MaxBodySize.
func (c *Client) getGeneric(dest string) ([]byte, error) {
	resp, err := c.do(c.httpClient.Get(g.String(dest)))
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	// Redirects are already resolved here, so a bare 302 is anomalous. Accepting
	// it merely looked like redirect handling, and that is what hid session expiry.
	if int(resp.StatusCode) != http.StatusOK {
		return nil, fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	body, err := readBody(resp)
	if err != nil {
		return nil, err
	}

	// Transient by design: withRetry re-authenticates and runs the step again.
	if bouncedToLogin(resp) || !hasAuthMarker(resp, body) {
		return nil, fmt.Errorf("%w: %v served the login page", ErrSessionExpired, dest)
	}

	c.rememberURL(resp)

	return body, nil
}

// getGrades fetches all grades from all subjects and returns them as raw body bytes.
func (c *Client) getGrades() ([]byte, error) {
	return c.getGeneric(GradeAllURL)
}

// getClasses fetches all old and new classes and returns them as raw body bytes.
func (c *Client) getClasses() ([]byte, error) {
	return c.getGeneric(ClassURL)
}

// getCourses fetches all courses and returns them as raw body bytes.
func (c *Client) getCourses() ([]byte, error) {
	return c.getGeneric(CourseURL)
}

// getCourse fetches a portal-supplied course href, pinned to BaseURL's
// scheme+host. dest comes from untrusted portal HTML and ResolveReference
// silently swaps host for an absolute ref, so the pin blocks SSRF / cookie-jar
// exfiltration via a compromised or MITM'd portal.
func (c *Client) getCourse(dest string) ([]byte, error) {
	// Resolve relative URLs; naive concat would malform them.
	base, err := url.Parse(BaseURL)
	if err != nil {
		return nil, err
	}

	ref, err := url.Parse(dest)
	if err != nil {
		return nil, err
	}

	resolved := base.ResolveReference(ref)

	// Pin host and scheme to the portal: reject cross-host hrefs.
	if resolved.Scheme != base.Scheme || resolved.Host != base.Host {
		return nil, fmt.Errorf("%w: %q", ErrInvalidHost, resolved)
	}

	return c.getGeneric(resolved.String())
}

// getCalendar fetches all events from exams calendar in ICS format.
func (c *Client) getCalendar() (Events, error) {
	body, err := c.getGeneric(CalendarURL)
	if err != nil {
		return Events{}, err
	}

	d := goics.NewDecoder(bytes.NewReader(body))
	evs := Events{}

	if err = d.Decode(&evs); err != nil {
		return Events{}, err
	}

	return evs, nil
}

// doClassAction switches the active class. classID comes from untrusted portal
// HTML and is interpolated into a URL path, so it is validated against reClassID
// and PathEscape'd to block path-injection.
func (c *Client) doClassAction(classID string) error {
	if classID == "" || strings.Contains(classID, "..") || !reClassID.MatchString(classID) {
		return fmt.Errorf("%w: %q", ErrInvalidClassID, classID)
	}

	if _, err := c.getGeneric(fmt.Sprintf(ClassActionURL, url.PathEscape(classID))); err != nil {
		return err
	}

	// On success only: Login must not re-apply a rejected selection.
	c.activeClass = classID

	return nil
}
