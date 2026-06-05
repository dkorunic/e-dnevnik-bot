// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"time"

	fakeua "github.com/lib4u/fake-useragent"
)

const (
	Timeout  = 120 * time.Second                                                                                                 // site can get really slow sometimes
	ChromeUA = "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Mobile Safari/537.36" // ChromeUA is a user agent string mimicking Chrome on Android.
)

// NewClientWithContext creates new *Client, initializing HTTP Cookie Jar, context and username with password.
func NewClientWithContext(ctx context.Context, username, password string) (*Client, error) {
	// Cookie jar required for SSO and security cookies.
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	// Own transport so CloseConnections isolates per client, not the shared default pool.
	// Comma-ok: instrumentation may swap DefaultTransport for a non-*Transport wrapper.
	var transport http.RoundTripper
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = dt.Clone()
	}

	c := &Client{
		httpClient: &http.Client{
			Timeout:   Timeout,
			Jar:       jar,
			Transport: transport,
		},
		ctx:      ctx,
		username: username,
		password: password,
	}

	return c, nil
}

// Login attempts get CSRF Token and do SSO/SAML authentication with random User-Agent per session.
func (c *Client) Login() error {
	// Per-session randomised UA reduces bot fingerprinting.
	ua, err := fakeua.New()
	if err != nil || ua == nil {
		c.userAgent = ChromeUA
	} else {
		ua.SetFallback(ChromeUA)
		c.userAgent = ua.Filter().Chrome().Platform(fakeua.Desktop).Get()
	}

	if err := c.getCSRFToken(); err != nil {
		return err
	}

	return c.doSAMLRequest()
}

// GetClassEvents attempts to fetch all subjects and their grades, as well as all calendar events for exams in ICS
// format, returning raw grades listing body bytes, parsed exam events and optional error.
func (c *Client) GetClassEvents(classID string) ([]byte, Events, error) {
	// Switch session to the requested class before scraping.
	err := c.doClassAction(classID)
	if err != nil {
		return nil, Events{}, err
	}

	rawGrades, err := c.getGrades()
	if err != nil {
		return nil, Events{}, err
	}

	events, err := c.getCalendar()
	if err != nil {
		return nil, Events{}, err
	}

	return rawGrades, events, nil
}

// GetClasses attempts to fetch all courses where a student has been previously enlisted or still is (multiple
// active classes possible).
func (c *Client) GetClasses() ([]byte, error) {
	return c.getClasses()
}

// GetCourses attempts to fetch all active courses.
func (c *Client) GetCourses() ([]byte, error) {
	return c.getCourses()
}

// GetCourse fetches the course with the given destination URL and returns its
// raw body, or an error.
func (c *Client) GetCourse(dest string) ([]byte, error) {
	return c.getCourse(dest)
}

// CloseConnections closes all connections on its transport.
func (c *Client) CloseConnections() {
	c.httpClient.CloseIdleConnections()
}
