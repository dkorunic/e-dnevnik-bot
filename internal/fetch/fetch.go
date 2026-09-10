// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"context"
	"time"
)

const Timeout = 120 * time.Second // site can get really slow sometimes

// NewClientWithContext creates a *Client backed by the impersonating HTTP client.
func NewClientWithContext(ctx context.Context, username, password string) (*Client, error) {
	// Built before the HTTP client because the client's header middleware closes
	// over c to read the navigation chain.
	c := &Client{
		ctx:      ctx,
		username: username,
		password: password,
	}

	cli, err := newSurfClient(c)
	if err != nil {
		return nil, err
	}

	c.httpClient = cli

	return c, nil
}

// Login attempts get CSRF Token and do SSO/SAML authentication.
func (c *Client) Login() error {
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
