// @license
// Copyright (C) 2022  Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package fetch

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/PuerkitoBio/goquery"
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
)

var (
	ErrUnexpectedStatus = errors.New("unexpected status code")
	ErrCSRFToken        = errors.New("could not find CSRF token")
	ErrNilBody          = errors.New("client body is nil")
	ErrInvalidLogin     = errors.New("unable to login")
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
		return fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return err
	}

	var csrfTokenExists bool

	// csrf_token is hidden in the input form
	doc.Find(`form > input[name="csrf_token"]`).
		Each(func(_ int, s *goquery.Selection) {
			c.csrfToken, csrfTokenExists = s.Attr("value")
		})

	// drain rest of the body
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	if !csrfTokenExists {
		return fmt.Errorf("%w", ErrCSRFToken)
	}

	return nil
}

// doSAMLRequest goes through SSO/SAML authentication, getting SimpleSAMLSessionID SSO cookie and refreshing cnOcjene
// security cookie set in getCSRFToken() step.
func (c *Client) doSAMLRequest() error {
	u, err := url.Parse(LoginURL)
	if err != nil {
		return err
	}

	// POST data struct corresponding to input form fields
	data := url.Values{
		"username":   {c.username},
		"password":   {c.password},
		"csrf_token": {c.csrfToken},
	}

	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, u.String(), strings.NewReader(data.Encode()))
	if err != nil {
		return err
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", LoginURL)

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

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		return err
	}

	// check if this is a login error
	alertSel := doc.FindMatcher(goquery.Single("#page-wrapper > div.flash-messages > div.alert > p"))
	if alertSel.Length() > 0 {
		return fmt.Errorf("%w: %v", ErrInvalidLogin, alertSel.Text())
	}

	// drain rest of the body
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	// regular SSO response should have HTTP 302 status
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
		return fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
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
// The function returns the response body as a string, or an error.
func (c *Client) getGeneric(dest string) (string, error) {
	u, err := url.Parse(dest)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", LoginURL)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		select {
		case <-c.ctx.Done():
			return "", c.ctx.Err()
		default:
			return "", err
		}
	}

	if resp == nil || resp.Body == nil {
		return "", fmt.Errorf("%w", ErrNilBody)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
		return "", fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

// getGrades fetches all grades from all subjects and returns them as raw body string.
func (c *Client) getGrades() (string, error) {
	return c.getGeneric(GradeAllURL)
}

// getClasses fetches all old and new classes and returns them as a raw body string.
func (c *Client) getClasses() (string, error) {
	return c.getGeneric(ClassURL)
}

// getCourses fetches all courses and returns them as a raw body string.
func (c *Client) getCourses() (string, error) {
	return c.getGeneric(CourseURL)
}

// getCourse fetches the course with the given destination URL and returns its
// raw body, or an error.
//
// The function takes the destination URL as a parameter, prepends the base URL
// to it, and then calls `getGeneric` to fetch the content.
// If the request returns a non-2xx status, or if the response body is empty,
// the function returns an error.
//
// The function also handles context cancellation and returns the context's
// error in such a case.
func (c *Client) getCourse(dest string) (string, error) {
	dest = strings.Join([]string{BaseURL, dest}, "")

	return c.getGeneric(dest)
}

// getCalendar fetches all events from exams calendar in ICS format.
func (c *Client) getCalendar() (Events, error) {
	body, err := c.getGeneric(CalendarURL)
	if err != nil {
		return Events{}, err
	}

	// decode ICS events
	d := goics.NewDecoder(strings.NewReader(body))
	evs := Events{}

	if err = d.Decode(&evs); err != nil {
		return Events{}, err
	}

	return evs, nil
}

// doClassAction switches the active class to the given class ID, returning an error upon failure.
func (c *Client) doClassAction(classID string) error {
	_, err := c.getGeneric(fmt.Sprintf(ClassActionURL, classID))

	return err
}
