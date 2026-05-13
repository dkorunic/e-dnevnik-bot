// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"context"
	"net/http"
	"net/url"
	"time"
)

// Client structure holds all HTTP Client related fields.
//
//nolint:containedctx
type Client struct {
	httpClient *http.Client
	URL        *url.URL
	ctx        context.Context
	username   string
	password   string
	csrfToken  string
	userAgent  string
}

// Event structure holds ICS event-related fields.
type Event struct {
	Start                time.Time
	Description, Summary string
}

// Events is a slice of Event structure.
type Events []Event

// Class structure holds all active classes.
type Class struct {
	ID     string
	Name   string
	Year   string
	School string
}

// Classes is a slice of Class structure.
type Classes []Class

type Course struct {
	URL  string
	Name string
}

type Courses []Course
