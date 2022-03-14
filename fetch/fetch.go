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
	"context"
	"net/http"
	"net/http/cookiejar"
	"time"

	"github.com/corpix/uarand"
)

const (
	LoginURL    = "https://ocjene.skole.hr/login"
	GradeAllURL = "https://ocjene.skole.hr/grade/all"
	CalendarURL = "https://ocjene.skole.hr/exam/ical"
	Timeout     = 60 * time.Second // site can get really slow sometimes
)

// NewClientWithContext creates new *Client, initializing HTTP Cookie Jar, context and username with password.
func NewClientWithContext(ctx context.Context, username, password string) (*Client, error) {
	// Cookie Jar needed for SSO and security cookie checks
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	c := &Client{
		httpClient: &http.Client{
			Timeout: Timeout,
			Jar:     jar,
		},
		ctx:      ctx,
		username: username,
		password: password,
	}

	return c, nil
}

// GetResponse attempts to fetch CSRF Token, do SSO/SAML request to have proper security and authentication cookies
// populated and then proceeds to fetch all subjects and their grades, as well as all calendar events for exams in ICS
// format, returning raw grades listing body, parsed exam events and optional error.
func (c *Client) GetResponse() (string, Events, error) {
	// generate random User-Agent per fetch dialog
	c.userAgent = uarand.GetRandom()

	// get secret CSRF Token from /
	if err := c.getCSRFToken(); err != nil {
		return "", Events{}, err
	}

	// do SSO/SAML authentication
	if err := c.doSAMLRequest(); err != nil {
		return "", Events{}, err
	}

	// fetch all grades as raw string/body
	rawGrades, err := c.getGrades()
	if err != nil {
		return "", Events{}, err
	}

	// fetch all exam dates from ICS calendar
	events, err := c.getCalendar()
	if err != nil {
		return "", Events{}, err
	}

	// forse immediate close of idle connections as we are done
	c.httpClient.CloseIdleConnections()

	return rawGrades, events, nil
}
