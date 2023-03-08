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

var (
	ErrUnexpectedStatus = errors.New("unexpected status code")
	ErrCSRFToken        = errors.New("could not find CSRF token")
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
		Each(func(i int, s *goquery.Selection) {
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
	defer resp.Body.Close()

	// drain rest of the body
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	// regular SSO response should have HTTP 302 status
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
		return fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	return nil
}

// getGrades fetches all grades from all subjects and returns them as raw body string.
func (c *Client) getGrades() (string, error) {
	u, err := url.Parse(GradeAllURL)
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

// getCalendar fetches all events from exams calendar in ICS format.
func (c *Client) getCalendar() (Events, error) {
	u, err := url.Parse(CalendarURL)
	if err != nil {
		return Events{}, err
	}

	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return Events{}, err
	}

	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Referer", LoginURL)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		select {
		case <-c.ctx.Done():
			return Events{}, c.ctx.Err()
		default:
			return Events{}, err
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Events{}, fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Events{}, err
	}

	// decode ICS events
	d := goics.NewDecoder(strings.NewReader(string(body)))
	evs := Events{}

	if err = d.Decode(&evs); err != nil {
		return Events{}, err
	}

	return evs, nil
}

// getClasses fetches all old and new classes and returns them as a raw body string.
func (c *Client) getClasses() (string, error) {
	u, err := url.Parse(ClassURL)
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
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	return string(body), nil
}

func (c *Client) doClassAction(classID string) error {
	u, err := url.Parse(fmt.Sprintf(ClassActionURL, classID))
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(c.ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}

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
	defer resp.Body.Close()

	// regular /class_action responses are HTTP 200 or HTTP 302 with redirect to /course
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusFound {
		return fmt.Errorf("%w: %v", ErrUnexpectedStatus, resp.StatusCode)
	}

	// drain rest of the body
	io.Copy(io.Discard, resp.Body) //nolint:errcheck

	return err
}
