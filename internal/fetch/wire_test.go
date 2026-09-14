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
// Cache-Control is absent by choice: the capture carried it, but that run
// reached the form by typed URL, and address-bar navigation is itself a
// max-age=0 navigation — so a form submit's behaviour is unconfirmed.
//
// Regenerate with the recipe in CLAUDE.md; never edit to match a failure.
var chromeRedirectWireOrder = []string{
	"Host",
	"Connection",
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

// readRequestNames returns one request's header names in wire order. Reading
// the socket is the point: Go's Header map and CDP both canonicalise and sort,
// so order and casing exist nowhere else.
func readRequestNames(c net.Conn, respond func(method, path string) string) []string {
	defer c.Close()

	_ = c.SetDeadline(time.Now().Add(10 * time.Second))

	r := bufio.NewReader(c)

	var (
		names        []string
		method, path string
		bodyLen      int
	)

	for i := 0; ; i++ {
		line, err := r.ReadString('\n')
		if err != nil {
			return names
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

		name, value, _ := strings.Cut(line, ":")
		names = append(names, name)

		if strings.EqualFold(name, "content-length") {
			bodyLen, _ = strconv.Atoi(strings.TrimSpace(value))
		}
	}

	if bodyLen > 0 {
		_, _ = io.CopyN(io.Discard, r, int64(bodyLen))
	}

	_, _ = c.Write([]byte(respond(method, path)))

	return names
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

			hops <- readRequestNames(c, func(method, _ string) string {
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

	var got []string

	for range 2 {
		select {
		case got = <-hops: // keep the last: the redirect hop
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for both hops")
		}
	}

	if len(got) != len(chromeRedirectWireOrder) {
		t.Fatalf("redirect hop sent %d headers, Chrome sends %d\n got:  %v\n want: %v",
			len(got), len(chromeRedirectWireOrder), got, chromeRedirectWireOrder)
	}

	for i := range got {
		if !strings.EqualFold(got[i], chromeRedirectWireOrder[i]) {
			t.Errorf("header %d on the wire = %q, Chrome sends %q\n got:  %v\n want: %v",
				i, got[i], chromeRedirectWireOrder[i], got, chromeRedirectWireOrder)
		}
	}
}
