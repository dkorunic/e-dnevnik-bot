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
	ErrUnexpectedStatus = errors.New("unexpected status code")
	ErrCSRFToken        = errors.New("could not find CSRF token")
	ErrNilBody          = errors.New("client body is nil")
	ErrInvalidLogin     = errors.New("unable to login")
	ErrSessionExpired   = errors.New("portal session expired")
	ErrBodyTooLarge     = errors.New("response body exceeds size limit")
	ErrInvalidClassID   = errors.New("invalid class ID — refusing to construct URL")
	ErrInvalidHost      = errors.New("portal href resolves to non-portal host — refusing to fetch")

	selCsrfToken  = cascadia.MustCompile(`form > input[name="csrf_token"]`)
	selLoginAlert = cascadia.MustCompile("#page-wrapper > div.flash-messages > div.alert > p")

	// reClassID rejects non-URL-safe class IDs to block path-injection via a tampered portal.
	reClassID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// loggedInMarker sits on the user chrome of every authenticated HTML page; the
// login page never carries it.
var loggedInMarker = []byte("logged-in-user")

// bouncedToLogin reports whether a request ended at the login page, which is how
// a lapsed session surfaces: the portal 302s data endpoints to /login and
// http.Client follows, so callers would otherwise see a plain 200 of login HTML
// and read it as a quiet school day.
//
// Data endpoints only — the login POST targets LoginPath either way.
func bouncedToLogin(resp *http.Response) bool {
	return resp.Request != nil && resp.Request.URL != nil && resp.Request.URL.Path == LoginPath
}

// hasAuthMarker catches a login page served in place of content rather than
// redirected to, which bouncedToLogin cannot see.
//
// Substring test, not a parse: it can only err toward "authenticated", and that
// is the safe direction — a false "expired" would trigger a re-login storm.
// Non-HTML is exempt because a live calendar response carries no markers; that
// costs no coverage, since a lapsed session redirects even /exam/ical to HTML.
func hasAuthMarker(resp *http.Response, body []byte) bool {
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		return true
	}

	return bytes.Contains(body, loggedInMarker)
}

// getCSRFToken extracts CSRF Token value hidden in the input form, optionally also getting initial value of cnOcjene
// security cookie.
func (c *Client) getCSRFToken() error {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, LoginURL, nil)
	if err != nil {
		return err
	}

	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Accept-Language", AcceptLanguageHR)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		default:
			return err
		}
	}

	if resp == nil || resp.Body == nil {
		return fmt.Errorf("%w", ErrNilBody)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)

		return fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	// Cap input; matches getGeneric's MaxBodySize ceiling.
	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, MaxBodySize))
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
	data := url.Values{
		"username":   {c.username},
		"password":   {c.password},
		"csrf_token": {c.csrfToken},
	}

	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, LoginURL, strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", LoginURL)
	req.Header.Set("Accept-Language", AcceptLanguageHR)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		default:
			return err
		}
	}

	if resp == nil || resp.Body == nil {
		return fmt.Errorf("%w", ErrNilBody)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		_, _ = io.Copy(io.Discard, resp.Body)

		return fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	// Buffered, not streamed: read twice, for the alert selector and the marker
	// probe. Cap matches getGeneric's ceiling.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodySize))
	if err != nil {
		return err
	}

	doc, err := goquery.NewDocumentFromReader(bytes.NewReader(body))
	if err != nil {
		return err
	}

	// Portal surfaces login failures as a flash-message alert div.
	alertSel := doc.FindMatcher(selLoginAlert)
	if alertSel.Length() > 0 {
		return fmt.Errorf("%w: %v", ErrInvalidLogin, alertSel.Text())
	}

	// A rejected login is a 302 back to /login, not a 4xx, so the status check
	// above never fires and the alert markup is the only other guard — one markup
	// drift away from a wrong password looking like an empty scrape.
	//
	// ErrInvalidLogin, not ErrSessionExpired, so markPermanent stops the cycle
	// rather than retrying bad credentials into the portal's rate limiter.
	if !hasAuthMarker(resp, body) {
		return fmt.Errorf("%w: portal returned the login page without an error message", ErrInvalidLogin)
	}

	return nil
}

// getGeneric GETs dest with the session's headers and returns the body,
// capped at MaxBodySize (ErrBodyTooLarge past that). Non-2xx/302 responses and
// context cancellation return an error.
func (c *Client) getGeneric(dest string) ([]byte, error) {
	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, dest, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", LoginURL)
	req.Header.Set("Accept-Language", AcceptLanguageHR)

	resp, err := c.httpClient.Do(req)
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
	defer resp.Body.Close()

	// No StatusFound: http.Client resolves redirects before this point, so a bare
	// 302 is anomalous. Accepting it only read as if redirects were handled, which
	// is what hid the expired-session path below.
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)

		return nil, fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	// +1 byte lets us detect truncation without reading the whole body.
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodySize+1))
	if err != nil {
		return nil, err
	}

	if len(body) > MaxBodySize {
		// Drain remainder so the connection can be reused.
		_, _ = io.Copy(io.Discard, resp.Body)

		return nil, fmt.Errorf("%w: %v", ErrBodyTooLarge, resp.Request.URL)
	}

	// Transient by design: withRetry re-authenticates and runs the step again.
	if bouncedToLogin(resp) || !hasAuthMarker(resp, body) {
		return nil, fmt.Errorf("%w: %v served the login page", ErrSessionExpired, dest)
	}

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

	_, err := c.getGeneric(fmt.Sprintf(ClassActionURL, url.PathEscape(classID)))

	return err
}
