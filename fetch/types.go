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
