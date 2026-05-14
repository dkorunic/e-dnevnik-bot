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
	ErrBodyTooLarge     = errors.New("response body exceeds size limit")
	ErrInvalidClassID   = errors.New("invalid class ID — refusing to construct URL")
	ErrInvalidHost      = errors.New("portal href resolves to non-portal host — refusing to fetch")

	selCsrfToken  = cascadia.MustCompile(`form > input[name="csrf_token"]`)
	selLoginAlert = cascadia.MustCompile("#page-wrapper > div.flash-messages > div.alert > p")

	// reClassID rejects non-URL-safe class IDs to block path-injection via a tampered portal.
	reClassID = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

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

	// Cap input; matches getGeneric's MaxBodySize ceiling.
	doc, err := goquery.NewDocumentFromReader(io.LimitReader(resp.Body, MaxBodySize))
	if err != nil {
		return err
	}

	// Portal surfaces login failures as a flash-message alert div.
	alertSel := doc.FindMatcher(selLoginAlert)
	if alertSel.Length() > 0 {
		return fmt.Errorf("%w: %v", ErrInvalidLogin, alertSel.Text())
	}

	return nil
}

// getGeneric performs a GET request on the given URL, handling the response
// with care.
//
// The function sets the User-Agent header to the value of `c.userAgent`, and
// Cache-Control and Pragma headers to "no-cache". It also sets the Referer
// header to `LoginURL`.
//
// If the request returns a non-2xx status, or if the response body is empty,
// the function returns an error.
//
// The function also handles context cancellation and returns the context's
// error in such a case.
//
// The function returns the response body as a byte slice, or an error.
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

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
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

// getCourse fetches the course with the given destination URL and returns its
// raw body, or an error.
//
// The destination originates from a portal-supplied <a href> attribute and is
// resolved against BaseURL via url.ResolveReference. ResolveReference happily
// switches host when ref is itself absolute, so the resolved URL is then
// host/scheme-pinned to BaseURL: a portal HTML that carries an external
// absolute href (compromised portal, MITM, future stored-XSS) would otherwise
// send the bot's authenticated cookie jar to an attacker-controlled host or be
// used as a SSRF probe against internal/cloud-metadata endpoints.
//
// The function also handles context cancellation and returns the context's
// error in such a case.
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

// doClassAction switches the active class to the given class ID, returning an error upon failure.
//
// The classID originates from a portal-supplied HTML attribute (`data-action-id`)
// and is interpolated into a URL path segment. To prevent path-injection through
// a tampered portal response, the value is validated against reClassID before
// use and additionally PathEscape'd as defence-in-depth.
func (c *Client) doClassAction(classID string) error {
	if classID == "" || strings.Contains(classID, "..") || !reClassID.MatchString(classID) {
		return fmt.Errorf("%w: %q", ErrInvalidClassID, classID)
	}

	_, err := c.getGeneric(fmt.Sprintf(ClassActionURL, url.PathEscape(classID)))

	return err
}
