// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package fetch

import (
	"bufio"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/enetx/g"
)

// chromeRedirectWireOrder is the header sequence a Chrome 152 wire capture
// shows on a POST→302→GET, in wire order.
//
// Cache-Control is here because the redirect inherits the POST's max-age=0
// (CLAUDE.md).
//
// Regenerate with the recipe in CLAUDE.md; never edit to match a failure.
// chromePostWireOrder is the same capture's login POST, where surf's profile
// scatters the client hints and inverts two pairs.
var chromePostWireOrder = []string{
	"Host",
	"Connection",
	"Content-Length",
	"Cache-Control",
	"Sec-Ch-Ua",
	"Sec-Ch-Ua-Mobile",
	"Sec-Ch-Ua-Platform",
	"Upgrade-Insecure-Requests",
	"Content-Type",
	"User-Agent",
	"Origin",
	"Accept",
	"Sec-Fetch-Site",
	"Sec-Fetch-Mode",
	"Sec-Fetch-User",
	"Sec-Fetch-Dest",
	"Referer",
	"Accept-Encoding",
	"Accept-Language",
}

var chromeRedirectWireOrder = []string{
	"Host",
	"Connection",
	"Cache-Control",
	"Upgrade-Insecure-Requests",
	"User-Agent",
	"Accept",
	"Sec-Fetch-Site",
	"Sec-Fetch-Mode",
	"Sec-Fetch-User",
	"Sec-Fetch-Dest",
	"Sec-Ch-Ua",
	"Sec-Ch-Ua-Mobile",
	"Sec-Ch-Ua-Platform",
	"Referer",
	"Accept-Encoding",
	"Accept-Language",
}

// readRequestHead returns one request's header lines in wire order. Reading the
// socket is the point: Go's Header map and CDP both canonicalise and sort, so
// order and casing exist nowhere else.
func readRequestHead(c net.Conn, respond func(method, path string) string) []string {
	defer c.Close()

	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	r := bufio.NewReader(c)

	var (
		head         []string
		method, path string
		bodyLen      int
	)

	for i := 0; ; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			return head
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}

		if i == 0 {
			if f := strings.Fields(line); len(f) >= 2 {
				method, path = f[0], f[1]
			}

			continue
		}

		head = append(head, line)

		name, value, _ := strings.Cut(line, ":")

		if strings.EqualFold(name, "content-length") {
			bodyLen, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}

	if bodyLen > 0 {
		_, _ = io.CopyN(io.Discard, r, int64(bodyLen))
	}

	_, _ = c.Write([]byte(respond(method, path)))

	return head
}

// headerNames reduces wire header lines to their names, in order.
func headerNames(head []string) []string {
	names := make([]string, 0, len(head))

	for _, l := range head {
		name, _, _ := strings.Cut(l, ":")
		names = append(names, name)
	}

	return names
}

// headerValue returns the value of name from wire header lines, or "".
func headerValue(head, name string) string {
	for l := range strings.SplitSeq(head, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if ok && strings.EqualFold(strings.TrimSpace(k), name) {
			return strings.TrimSpace(v)
		}
	}

	return ""
}

// TestRedirectHopMatchesChromeWireOrder drives the real transport at a raw
// listener and diffs the redirect hop against the Chrome capture. Asserting
// HeaderOrderKey would not do: the ordered writer injects Content-Length from
// the list alone, so the list can be right while the wire is wrong.
//
// Case-insensitive because enetx/http canonicalises every name at write time,
// so Chrome's lowercase sec-ch-* hints are unreachable from here (CLAUDE.md).
func TestRedirectHopMatchesChromeWireOrder(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer l.Close()

	hops := make(chan []string, 4)

	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}

			hops <- readRequestHead(c, func(method, _ string) string {
				if method == "POST" {
					return "HTTP/1.1 302 Found\r\nLocation: /class\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
				}

				const body = "<html>logged-in-user</html>"

				return "HTTP/1.1 200 OK\r\nContent-Type: text/html; charset=UTF-8\r\nContent-Length: " +
					strconv.Itoa(len(body)) + "\r\nConnection: close\r\n\r\n" + body
			})
		}
	}()

	c, err := NewClientWithContext(t.Context(), "user@skole.hr", "s3cret")
	if err != nil {
		t.Fatal(err)
	}

	defer c.CloseConnections()

	base := "http://" + l.Addr().String()

	// What getCSRFToken leaves behind, so the POST carries a Referer as in
	// production.
	c.lastURL = base + "/login"

	form := url.Values{"username": {"u"}, "password": {"p"}, "csrf_token": {"tok"}}.Encode()

	req := c.httpClient.Post(g.String(base + "/login")).Body(form)
	req.GetRequest().GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(form)), nil
	}

	resp, err := c.do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}

	defer resp.Body.Close()

	var post, redirected []string

	for i := range 2 {
		select {
		case head := <-hops:
			if i == 0 {
				post = head
			} else {
				redirected = head
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for both hops")
		}
	}

	postHead := strings.Join(post, "\n")

	if got := headerValue(postHead, "Cache-Control"); got != "max-age=0" {
		t.Errorf("login POST sent Cache-Control %q, Chrome sends \"max-age=0\"", got)
	}

	if got := headerValue(postHead, "Pragma"); got != "" {
		t.Errorf("login POST sent Pragma %q; Chrome sends none on any hop", got)
	}

	assertWireOrder(t, "login POST", headerNames(post), chromePostWireOrder)
	assertWireOrder(t, "redirect hop", headerNames(redirected), chromeRedirectWireOrder)
}

// assertWireOrder compares a captured header sequence name by name. Case is
// ignored: enetx/http canonicalises every name at write time, so Chrome's
// lowercase sec-ch-* hints are unreachable from here (see CLAUDE.md).
func assertWireOrder(t *testing.T, what string, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("%s sent %d headers, Chrome sends %d\n got:  %v\n want: %v",
			what, len(got), len(want), got, want)
	}

	for i := range got {
		if !strings.EqualFold(got[i], want[i]) {
			t.Errorf("%s header %d = %q, Chrome sends %q\n got:  %v\n want: %v",
				what, i, got[i], want[i], got, want)
		}
	}
}
