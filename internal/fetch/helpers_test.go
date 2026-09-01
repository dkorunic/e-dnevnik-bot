// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// roundTripFunc adapts a function to http.RoundTripper. The portal URLs are
// compile-time constants, so a stub transport — not httptest.Server — is the
// only way to exercise these paths offline.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// newStubClient builds a Client wired to rt, bypassing NewClientWithContext's
// real transport. Fields are unexported, hence the white-box package.
func newStubClient(ctx context.Context, rt http.RoundTripper) *Client {
	return &Client{
		httpClient: &http.Client{Transport: rt},
		ctx:        ctx,
		username:   "user@skole.hr",
		password:   "s3cret",
		userAgent:  ChromeUA,
	}
}

// stringResponse builds a canned 200-style response with the given status.
func stringResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}
}

const loginPageWithToken = `<html><body>
<form method="post"><input type="hidden" name="csrf_token" value="tok-12345"></form>
</body></html>`

// TestGetCSRFToken covers the token-extraction contract and every failure mode
// that must surface as a typed error rather than an empty token.
func TestGetCSRFToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		status    int
		body      string
		transErr  error
		wantErr   error
		wantToken string
	}{
		{
			name:      "token extracted from hidden input",
			status:    http.StatusOK,
			body:      loginPageWithToken,
			wantToken: "tok-12345",
		},
		{
			name:    "form without csrf_token input",
			status:  http.StatusOK,
			body:    `<html><body><form method="post"></form></body></html>`,
			wantErr: ErrCSRFToken,
		},
		{
			name:    "input outside a form is not matched",
			status:  http.StatusOK,
			body:    `<html><body><input name="csrf_token" value="loose"></body></html>`,
			wantErr: ErrCSRFToken,
		},
		{
			name:    "non-200 status",
			status:  http.StatusServiceUnavailable,
			body:    "maintenance",
			wantErr: ErrUnexpectedStatus,
		},
		{
			name:     "transport failure propagates",
			transErr: errors.New("dial tcp: connection refused"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if tt.transErr != nil {
					return nil, tt.transErr
				}

				if req.URL.String() != LoginURL {
					t.Errorf("requested %v, want %v", req.URL, LoginURL)
				}

				if got := req.Header.Get("Accept-Language"); got != AcceptLanguageHR {
					t.Errorf("Accept-Language = %q, want %q — the alert matcher depends on Croatian responses", got, AcceptLanguageHR)
				}

				return stringResponse(req, tt.status, tt.body), nil
			}))

			err := c.getCSRFToken()

			switch {
			case tt.wantToken != "":
				if err != nil {
					t.Fatalf("getCSRFToken() = %v, want nil", err)
				}

				if c.csrfToken != tt.wantToken {
					t.Errorf("csrfToken = %q, want %q", c.csrfToken, tt.wantToken)
				}
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("getCSRFToken() = %v, want %v", err, tt.wantErr)
				}
			default:
				if err == nil {
					t.Fatal("getCSRFToken() = nil, want a transport error")
				}
			}
		})
	}
}

// TestGetCSRFTokenCancelledContext verifies the select-on-ctx.Done branch: a
// transport failure caused by shutdown must report context.Canceled so callers
// can distinguish it from a genuine portal fault and skip retrying.
func TestGetCSRFTokenCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	c := newStubClient(ctx, roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("request cancelled")
	}))

	if err := c.getCSRFToken(); !errors.Is(err, context.Canceled) {
		t.Fatalf("getCSRFToken() = %v, want context.Canceled", err)
	}
}

// TestDoSAMLRequestPostsCredentials asserts the login POST actually carries the
// username, password and the token captured by getCSRFToken. A silently empty
// csrf_token would be accepted by a response-only test but rejected by the portal.
func TestDoSAMLRequestPostsCredentials(t *testing.T) {
	t.Parallel()

	var gotForm url.Values

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Fatalf("reading request body: %v", err)
		}

		gotForm, err = url.ParseQuery(string(body))
		if err != nil {
			t.Fatalf("parsing form body: %v", err)
		}

		if req.Method != http.MethodPost {
			t.Errorf("method = %v, want POST", req.Method)
		}

		if ct := req.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want application/x-www-form-urlencoded", ct)
		}

		return stringResponse(req, http.StatusOK, `<html><body>Dobrodošli</body></html>`), nil
	}))
	c.csrfToken = "tok-12345"

	if err := c.doSAMLRequest(); err != nil {
		t.Fatalf("doSAMLRequest() = %v, want nil", err)
	}

	for field, want := range map[string]string{
		"username":   "user@skole.hr",
		"password":   "s3cret",
		"csrf_token": "tok-12345",
	} {
		if got := gotForm.Get(field); got != want {
			t.Errorf("form field %q = %q, want %q", field, got, want)
		}
	}
}

// TestDoSAMLRequestErrors covers login rejection and status handling. Bad
// credentials arrive as HTTP 200 with a flash-message alert div, so status
// alone cannot detect them — the selector match is the whole signal.
func TestDoSAMLRequestErrors(t *testing.T) {
	t.Parallel()

	const alertPage = `<html><body><div id="page-wrapper"><div class="flash-messages">` +
		`<div class="alert"><p>Neispravno korisničko ime ili lozinka</p></div></div></div></body></html>`

	tests := []struct {
		name    string
		status  int
		body    string
		wantErr error
	}{
		{
			name:    "alert div on HTTP 200 means bad credentials",
			status:  http.StatusOK,
			body:    alertPage,
			wantErr: ErrInvalidLogin,
		},
		{
			name:    "4xx is an unexpected status",
			status:  http.StatusForbidden,
			body:    "denied",
			wantErr: ErrUnexpectedStatus,
		},
		{
			name:    "5xx is an unexpected status",
			status:  http.StatusBadGateway,
			body:    "bad gateway",
			wantErr: ErrUnexpectedStatus,
		},
		{
			name:   "302 redirect is a successful login",
			status: http.StatusFound,
			body:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return stringResponse(req, tt.status, tt.body), nil
			}))

			err := c.doSAMLRequest()

			if tt.wantErr == nil {
				if err != nil {
					t.Fatalf("doSAMLRequest() = %v, want nil", err)
				}

				return
			}

			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("doSAMLRequest() = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// TestDoSAMLRequestInvalidLoginIncludesAlertText checks the portal's message is
// carried into the error — it is the only diagnostic an operator gets for a
// locked or expired AAI account.
func TestDoSAMLRequestInvalidLoginIncludesAlertText(t *testing.T) {
	t.Parallel()

	const msg = "Korisnički račun je zaključan"

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return stringResponse(req, http.StatusOK,
			`<html><body><div id="page-wrapper"><div class="flash-messages">`+
				`<div class="alert"><p>`+msg+`</p></div></div></div></body></html>`), nil
	}))

	err := c.doSAMLRequest()
	if !errors.Is(err, ErrInvalidLogin) {
		t.Fatalf("doSAMLRequest() = %v, want ErrInvalidLogin", err)
	}

	if !strings.Contains(err.Error(), msg) {
		t.Errorf("error %q does not carry the portal alert text %q", err, msg)
	}
}

// TestGetGenericStatusHandling pins the accepted status set. 302 is accepted
// because the portal redirects after a class switch; anything else must error
// rather than hand a login page back to the parser as if it were grades.
func TestGetGenericStatusHandling(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{name: "200 OK", status: http.StatusOK, body: "payload"},
		{name: "302 Found is accepted", status: http.StatusFound, body: "redirect body"},
		{name: "301 is rejected", status: http.StatusMovedPermanently, body: "", wantErr: true},
		{name: "404 is rejected", status: http.StatusNotFound, body: "", wantErr: true},
		{name: "500 is rejected", status: http.StatusInternalServerError, body: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return stringResponse(req, tt.status, tt.body), nil
			}))

			body, err := c.getGeneric(GradeAllURL)

			if tt.wantErr {
				if !errors.Is(err, ErrUnexpectedStatus) {
					t.Fatalf("getGeneric() = %v, want ErrUnexpectedStatus", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("getGeneric() = %v, want nil", err)
			}

			if string(body) != tt.body {
				t.Errorf("getGeneric() body = %q, want %q", body, tt.body)
			}
		})
	}
}

// TestGetGenericBodylessResponse documents that a bodyless response is safe.
// The ErrNilBody guard is defence in depth only: http.Client.send substitutes a
// no-op body before returning, so a transport handing back resp.Body == nil is
// normalised to an empty body rather than reaching the guard. This test pins
// the observable behaviour — empty body, no error, no nil-panic at Body.Close —
// so a future change to that normalisation surfaces here instead of in a panic.
func TestGetGenericBodylessResponse(t *testing.T) {
	t.Parallel()

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Request: req}, nil
	}))

	body, err := c.getGeneric(GradeAllURL)
	if err != nil && !errors.Is(err, ErrNilBody) {
		t.Fatalf("getGeneric() = %v, want nil or ErrNilBody", err)
	}

	if len(body) != 0 {
		t.Errorf("getGeneric() body = %q, want empty", body)
	}
}

// zeroReader yields an endless stream of 'x' without allocating, so the
// oversize-body test can exceed MaxBodySize without a 32 MiB fixture.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}

	return len(p), nil
}

// TestGetGenericBodyTooLarge drives the MaxBodySize ceiling. The read is capped
// at MaxBodySize+1 precisely so truncation is detectable without buffering the
// whole (potentially unbounded) response.
func TestGetGenericBodyTooLarge(t *testing.T) {
	t.Parallel()

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(io.LimitReader(zeroReader{}, MaxBodySize+1024)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}))

	if _, err := c.getGeneric(GradeAllURL); !errors.Is(err, ErrBodyTooLarge) {
		t.Fatalf("getGeneric() = %v, want ErrBodyTooLarge", err)
	}
}

// TestGetGenericExactlyMaxBodySize checks the boundary is inclusive: a body of
// exactly MaxBodySize must be accepted, not rejected by an off-by-one.
func TestGetGenericExactlyMaxBodySize(t *testing.T) {
	t.Parallel()

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(io.LimitReader(zeroReader{}, MaxBodySize)),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}))

	body, err := c.getGeneric(GradeAllURL)
	if err != nil {
		t.Fatalf("getGeneric() = %v, want nil at exactly MaxBodySize", err)
	}

	if len(body) != MaxBodySize {
		t.Errorf("body length = %d, want %d", len(body), MaxBodySize)
	}
}

// TestGetGenericCancelledContext verifies shutdown-induced failures surface as
// context.Canceled rather than an opaque transport error.
func TestGetGenericCancelledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	c := newStubClient(ctx, roundTripFunc(func(_ *http.Request) (*http.Response, error) {
		return nil, errors.New("cancelled")
	}))

	if _, err := c.getGeneric(GradeAllURL); !errors.Is(err, context.Canceled) {
		t.Fatalf("getGeneric() = %v, want context.Canceled", err)
	}
}

// TestGetGenericTargets confirms the thin wrappers each hit their own endpoint;
// a copy-paste swap between them would silently scrape the wrong page.
func TestGetGenericTargets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(*Client) ([]byte, error)
		want string
	}{
		{name: "getGrades", call: (*Client).getGrades, want: GradeAllURL},
		{name: "getClasses", call: (*Client).getClasses, want: ClassURL},
		{name: "getCourses", call: (*Client).getCourses, want: CourseURL},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got string

			c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
				got = req.URL.String()

				return stringResponse(req, http.StatusOK, "ok"), nil
			}))

			if _, err := tt.call(c); err != nil {
				t.Fatalf("%v() = %v, want nil", tt.name, err)
			}

			if got != tt.want {
				t.Errorf("%v() requested %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

// TestDoClassActionRejectsInjection is the security-critical case: classID comes
// from untrusted portal HTML and is interpolated into a URL path. Any input that
// escapes the reClassID charset must be rejected *before* a request is made.
func TestDoClassActionRejectsInjection(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		classID string
	}{
		{name: "empty", classID: ""},
		{name: "parent directory traversal", classID: "../../admin"},
		{name: "bare dot-dot", classID: ".."},
		{name: "path separator", classID: "123/course"},
		{name: "query injection", classID: "123?x=1"},
		{name: "fragment injection", classID: "123#frag"},
		{name: "absolute url", classID: "https://evil.example.com/"},
		{name: "percent encoded traversal", classID: "%2e%2e%2fadmin"},
		{name: "whitespace", classID: "123 456"},
		{name: "newline injection", classID: "123\nHost: evil"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var called bool

			c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true

				return stringResponse(req, http.StatusOK, "ok"), nil
			}))

			err := c.doClassAction(tt.classID)
			if !errors.Is(err, ErrInvalidClassID) {
				t.Fatalf("doClassAction(%q) = %v, want ErrInvalidClassID", tt.classID, err)
			}

			if called {
				t.Errorf("doClassAction(%q) issued an HTTP request; validation must short-circuit first", tt.classID)
			}
		})
	}
}

// TestDoClassActionAcceptsValidID confirms a well-formed ID is interpolated into
// the class-action URL rather than rejected by an over-tight charset.
func TestDoClassActionAcceptsValidID(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		classID string
	}{
		{name: "numeric", classID: "123456"},
		{name: "alphanumeric", classID: "abc123XYZ"},
		{name: "with dash", classID: "class-2024"},
		{name: "with underscore", classID: "class_2024"},
		{name: "with dot", classID: "class.2024"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got string

			c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
				got = req.URL.String()

				return stringResponse(req, http.StatusOK, "ok"), nil
			}))

			if err := c.doClassAction(tt.classID); err != nil {
				t.Fatalf("doClassAction(%q) = %v, want nil", tt.classID, err)
			}

			want := "https://ocjene.skole.hr/class_action/" + tt.classID + "/course"
			if got != want {
				t.Errorf("doClassAction(%q) requested %v, want %v", tt.classID, got, want)
			}
		})
	}
}

// TestGetCalendarDecodesICS covers the ICS decode path end to end, including the
// portal's own VEVENT shape.
func TestGetCalendarDecodesICS(t *testing.T) {
	t.Parallel()

	const ics = "BEGIN:VCALENDAR\r\n" +
		"VERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\n" +
		"DTSTART:20250401T080000Z\r\n" +
		"SUMMARY:Matematika\r\n" +
		"DESCRIPTION:Pisana provjera\r\n" +
		"END:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	var got string

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.URL.String()

		return stringResponse(req, http.StatusOK, ics), nil
	}))

	events, err := c.getCalendar()
	if err != nil {
		t.Fatalf("getCalendar() = %v, want nil", err)
	}

	if got != CalendarURL {
		t.Errorf("getCalendar() requested %v, want %v", got, CalendarURL)
	}

	if len(events) != 1 {
		t.Fatalf("getCalendar() returned %d events, want 1: %+v", len(events), events)
	}

	if events[0].Summary != "Matematika" || events[0].Description != "Pisana provjera" {
		t.Errorf("event = %+v, want summary/description from the VEVENT", events[0])
	}

	if events[0].Start.IsZero() {
		t.Error("event Start is zero; DTSTART was not parsed")
	}
}

// TestGetCalendarFetchError checks a transport-level failure returns an empty
// Events rather than a partially populated one the scraper would treat as real.
func TestGetCalendarFetchError(t *testing.T) {
	t.Parallel()

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return stringResponse(req, http.StatusInternalServerError, ""), nil
	}))

	events, err := c.getCalendar()
	if !errors.Is(err, ErrUnexpectedStatus) {
		t.Fatalf("getCalendar() = %v, want ErrUnexpectedStatus", err)
	}

	if len(events) != 0 {
		t.Errorf("getCalendar() returned %d events on error, want 0", len(events))
	}
}

// TestLoginSequence verifies Login runs getCSRFToken then doSAMLRequest in that
// order, carrying the token from the first into the second.
func TestLoginSequence(t *testing.T) {
	t.Parallel()

	var (
		mu     sync.Mutex
		calls  []string
		posted url.Values
	)

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()

		calls = append(calls, req.Method)

		if req.Method == http.MethodGet {
			return stringResponse(req, http.StatusOK, loginPageWithToken), nil
		}

		body, _ := io.ReadAll(req.Body)
		posted, _ = url.ParseQuery(string(body))

		return stringResponse(req, http.StatusOK, "<html><body>ok</body></html>"), nil
	}))

	if err := c.Login(); err != nil {
		t.Fatalf("Login() = %v, want nil", err)
	}

	if len(calls) != 2 || calls[0] != http.MethodGet || calls[1] != http.MethodPost {
		t.Fatalf("Login() made calls %v, want [GET POST]", calls)
	}

	if posted.Get("csrf_token") != "tok-12345" {
		t.Errorf("login POST csrf_token = %q, want the token scraped by getCSRFToken", posted.Get("csrf_token"))
	}
}

// TestLoginStopsOnCSRFFailure verifies Login short-circuits: without a token the
// POST would submit an empty csrf_token and trip the portal's rate limiter.
func TestLoginStopsOnCSRFFailure(t *testing.T) {
	t.Parallel()

	var posts int

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPost {
			posts++
		}

		return stringResponse(req, http.StatusOK, "<html><body>no form here</body></html>"), nil
	}))

	if err := c.Login(); !errors.Is(err, ErrCSRFToken) {
		t.Fatalf("Login() = %v, want ErrCSRFToken", err)
	}

	if posts != 0 {
		t.Errorf("Login() issued %d POSTs after CSRF failure, want 0", posts)
	}
}

// TestGetClassEventsRejectsBadClassID checks the injection guard holds at the
// exported boundary too, and that no grade/calendar fetch follows a rejection.
func TestGetClassEventsRejectsBadClassID(t *testing.T) {
	t.Parallel()

	var called bool

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		called = true

		return stringResponse(req, http.StatusOK, "ok"), nil
	}))

	_, events, err := c.GetClassEvents("../etc/passwd")
	if !errors.Is(err, ErrInvalidClassID) {
		t.Fatalf("GetClassEvents() = %v, want ErrInvalidClassID", err)
	}

	if called {
		t.Error("GetClassEvents() issued an HTTP request for an invalid class ID")
	}

	if len(events) != 0 {
		t.Errorf("GetClassEvents() returned %d events on error, want 0", len(events))
	}
}

// TestGetClassEventsHappyPath walks the full per-class sequence: switch class,
// fetch grades, fetch calendar — in that order.
func TestGetClassEventsHappyPath(t *testing.T) {
	t.Parallel()

	const ics = "BEGIN:VCALENDAR\r\nVERSION:2.0\r\n" +
		"BEGIN:VEVENT\r\nDTSTART:20250401T080000Z\r\nSUMMARY:Fizika\r\nDESCRIPTION:Ispit\r\nEND:VEVENT\r\n" +
		"END:VCALENDAR\r\n"

	var order []string

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		order = append(order, req.URL.Path)

		if req.URL.String() == CalendarURL {
			return stringResponse(req, http.StatusOK, ics), nil
		}

		return stringResponse(req, http.StatusOK, "<html>grades</html>"), nil
	}))

	raw, events, err := c.GetClassEvents("42")
	if err != nil {
		t.Fatalf("GetClassEvents() = %v, want nil", err)
	}

	want := []string{"/class_action/42/course", "/grade/all", "/exam/ical"}
	if len(order) != len(want) {
		t.Fatalf("GetClassEvents() requested %v, want %v", order, want)
	}

	for i := range want {
		if order[i] != want[i] {
			t.Errorf("request %d was %v, want %v", i, order[i], want[i])
		}
	}

	if string(raw) != "<html>grades</html>" {
		t.Errorf("raw grades = %q, want the grade page body", raw)
	}

	if len(events) != 1 || events[0].Summary != "Fizika" {
		t.Errorf("events = %+v, want the single decoded VEVENT", events)
	}
}

// TestGetCourseResolvesRelativeHref checks a portal-relative href is resolved
// against BaseURL rather than passed through as an unusable relative URL.
func TestGetCourseResolvesRelativeHref(t *testing.T) {
	t.Parallel()

	var got string

	c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
		got = req.URL.String()

		return stringResponse(req, http.StatusOK, "course body"), nil
	}))

	body, err := c.getCourse("/course/12345")
	if err != nil {
		t.Fatalf("getCourse() = %v, want nil", err)
	}

	if got != "https://ocjene.skole.hr/course/12345" {
		t.Errorf("getCourse() requested %v, want it resolved against BaseURL", got)
	}

	if string(body) != "course body" {
		t.Errorf("getCourse() body = %q, want %q", body, "course body")
	}
}

// TestExportedGettersHitTheirEndpoints covers the exported wrappers. They are
// one-liners, but a copy-paste swap between them would silently scrape the
// wrong page and surface as missing alerts rather than as an error.
func TestExportedGettersHitTheirEndpoints(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		call func(*Client) ([]byte, error)
		want string
	}{
		{name: "GetClasses", call: (*Client).GetClasses, want: ClassURL},
		{name: "GetCourses", call: (*Client).GetCourses, want: CourseURL},
		{
			name: "GetCourse",
			call: func(c *Client) ([]byte, error) { return c.GetCourse("/course/999") },
			want: "https://ocjene.skole.hr/course/999",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var got string

			c := newStubClient(t.Context(), roundTripFunc(func(req *http.Request) (*http.Response, error) {
				got = req.URL.String()

				return stringResponse(req, http.StatusOK, "body"), nil
			}))

			body, err := tt.call(c)
			if err != nil {
				t.Fatalf("%v() = %v, want nil", tt.name, err)
			}

			if got != tt.want {
				t.Errorf("%v() requested %v, want %v", tt.name, got, tt.want)
			}

			if string(body) != "body" {
				t.Errorf("%v() body = %q, want %q", tt.name, body, "body")
			}
		})
	}
}
