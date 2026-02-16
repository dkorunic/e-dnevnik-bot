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

	fakeua "github.com/lib4u/fake-useragent"
)

const (
	Timeout  = 60 * time.Second                                                                                                  // site can get really slow sometimes
	ChromeUA = "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36" // ChromeUA is a user agent string mimicking Chrome on Android.
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

// Login attempts get CSRF Token and do SSO/SAML authentication with random User-Agent per session.
func (c *Client) Login() error {
	// generate random User-Agent per fetch dialog
	ua, _ := fakeua.New()
	ua.SetFallback(ChromeUA)
	c.userAgent = ua.Filter().Chrome().Platform(fakeua.Desktop).Get()

	// get secret CSRF Token from /
	if err := c.getCSRFToken(); err != nil {
		return err
	}

	// do SSO/SAML authentication
	return c.doSAMLRequest()
}

// GetClassEvents attempts to fetch all subjects and their grades, as well as all calendar events for exams in ICS
// format, returning raw grades listing body, parsed exam events and optional error.
func (c *Client) GetClassEvents(classID string) (string, Events, error) {
	// do class action to switch active class to class ID
	err := c.doClassAction(classID)
	if err != nil {
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

	return rawGrades, events, nil
}

// GetClasses attempts to fetch all courses where a student has been previously enlisted or still is (multiple
// active classes possible).
func (c *Client) GetClasses() (string, error) {
	// fetch all active classes
	rawClasses, err := c.getClasses()
	if err != nil {
		return "", err
	}

	return rawClasses, nil
}

// GetCourses attempts to fetch all active courses.
func (c *Client) GetCourses() (string, error) {
	// fetch all active courses
	rawCourses, err := c.getCourses()
	if err != nil {
		return "", err
	}

	return rawCourses, nil
}

// GetCourse fetches the course with the given destination URL and returns its
// raw body, or an error.
func (c *Client) GetCourse(dest string) (string, error) {
	return c.getCourse(dest)
}

// CloseConnections closes all connections on its transport.
func (c *Client) CloseConnections() {
	c.httpClient.CloseIdleConnections()
}
