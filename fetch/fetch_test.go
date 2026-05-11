// @license
// Copyright (C) 2025 Dinko Korunic
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
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jordic/goics"
)

func TestParseFirstDateTime(t *testing.T) {
	t.Parallel()
	testCases := []struct {
		name      string
		value     string
		expected  time.Time
		expectErr bool
	}{
		{
			name:      "LayoutISO8601CompactZ",
			value:     "20250101T120000Z",
			expected:  time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
			expectErr: false,
		},
		{
			name:      "LayoutISO8601CompactNoTZ",
			value:     "20250101T120000",
			expected:  time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
			expectErr: false,
		},
		{
			name:      "LayoutISO8601Short",
			value:     "20250101",
			expected:  time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC),
			expectErr: false,
		},
		{
			name:      "InvalidFormat",
			value:     "invalid-date",
			expectErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			actual, err := parseFirstDateTime([]string{LayoutISO8601CompactZ, LayoutISO8601CompactNoTZ, LayoutISO8601Short}, tc.value)
			if (err != nil) != tc.expectErr {
				t.Fatalf("expected error: %v, got: %v", tc.expectErr, err)
			}
			if !tc.expectErr && !actual.Equal(tc.expected) {
				t.Errorf("expected: %v, got: %v", tc.expected, actual)
			}
		})
	}
}

func TestConsumeICal(t *testing.T) {
	t.Parallel()
	validEvent := goics.Event{
		Data: map[string]*goics.IcsNode{
			EventDateStart:   {Val: "20250101T120000Z"},
			EventDescription: {Val: "Test Description"},
			EventSummary:     {Val: "Test Summary"},
		},
	}

	incompleteEvent := goics.Event{
		Data: map[string]*goics.IcsNode{
			EventDescription: {Val: "Test Description"},
			EventSummary:     {Val: "Test Summary"},
		},
	}

	invalidDateEvent := goics.Event{
		Data: map[string]*goics.IcsNode{
			EventDateStart:   {Val: "invalid-date"},
			EventDescription: {Val: "Test Description"},
			EventSummary:     {Val: "Test Summary"},
		},
	}

	cal := &goics.Calendar{
		Events: []*goics.Event{&validEvent, &incompleteEvent, &invalidDateEvent},
	}

	events := &Events{}
	err := events.ConsumeICal(cal, nil)
	if err != nil {
		t.Fatalf("ConsumeICal failed: %v", err)
	}

	if len(*events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(*events))
	}

	expectedEvent := Event{
		Start:       time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC),
		Description: "Test Description",
		Summary:     "Test Summary",
	}

	if !reflect.DeepEqual((*events)[0], expectedEvent) {
		t.Errorf("expected event: %v, got: %v", expectedEvent, (*events)[0])
	}
}

func TestNewClientWithContext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, err := NewClientWithContext(ctx, "user@example.com", "password")
	if err != nil {
		t.Fatalf("NewClientWithContext() failed: %v", err)
	}

	if client == nil {
		t.Fatal("NewClientWithContext() returned nil client")
	}

	if client.httpClient == nil {
		t.Error("NewClientWithContext() returned client with nil httpClient")
	}

	if client.username != "user@example.com" {
		t.Errorf("expected username %q, got %q", "user@example.com", client.username)
	}
}

func TestCloseConnections(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	client, err := NewClientWithContext(ctx, "user@example.com", "password")
	if err != nil {
		t.Fatalf("NewClientWithContext() failed: %v", err)
	}

	client.CloseConnections()
}

func TestConsumeICalEmptyCalendar(t *testing.T) {
	t.Parallel()
	cal := &goics.Calendar{Events: []*goics.Event{}}
	events := &Events{}

	if err := events.ConsumeICal(cal, nil); err != nil {
		t.Fatalf("ConsumeICal() on empty calendar failed: %v", err)
	}

	if len(*events) != 0 {
		t.Errorf("expected 0 events from empty calendar, got %d", len(*events))
	}
}

// TestParseFirstDateTimeUsesUTCForNoTZLayouts asserts that layouts without
// embedded timezone info are parsed as time.UTC so the dedup hash and the
// displayed exam date stay stable across hosts in different timezones. Parsing
// in time.Local would shift all-day events by up to ±14 h depending on the
// server's locale, changing the hash key and showing the wrong calendar day.
func TestParseFirstDateTimeUsesUTCForNoTZLayouts(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		layout string
		value  string
	}{
		{"compact-no-tz", LayoutISO8601CompactNoTZ, "20250612T080000"},
		{"date-only", LayoutISO8601Short, "20250612"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := parseFirstDateTime([]string{tc.layout}, tc.value)
			if err != nil {
				t.Fatalf("parseFirstDateTime(%q) unexpected error: %v", tc.value, err)
			}

			if got.Location() != time.UTC {
				t.Errorf("parseFirstDateTime(%q) location = %v, want time.UTC", tc.value, got.Location())
			}
		})
	}
}

// TestParseFirstDateTimeUTCForTZLayouts verifies the counterpart: layouts that
// embed timezone info (Z/offset) must NOT force time.Local — the parsed result
// should honour the embedded offset (UTC in the Z case).
func TestParseFirstDateTimeUTCForTZLayouts(t *testing.T) {
	t.Parallel()

	got, err := parseFirstDateTime([]string{LayoutISO8601CompactZ}, "20250612T120000Z")
	if err != nil {
		t.Fatalf("parseFirstDateTime unexpected error: %v", err)
	}

	if got.UTC().Hour() != 12 {
		t.Errorf("parseFirstDateTime Z-suffix: expected hour 12 UTC, got %d", got.UTC().Hour())
	}
}

// TestGetCourseRejectsCrossHost verifies the SSRF defence: getCourse must
// refuse to fetch any href whose resolved host or scheme differs from
// BaseURL. Without this guard a compromised/MITMed portal can hand the bot
// an absolute URL pointing to an attacker-controlled host or an internal /
// cloud-metadata endpoint — and url.ResolveReference returns ref unchanged
// when ref is itself absolute, swapping the host silently.
func TestGetCourseRejectsCrossHost(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	client, err := NewClientWithContext(ctx, "user@example.com", "password")
	if err != nil {
		t.Fatalf("NewClientWithContext() failed: %v", err)
	}

	cases := []struct {
		name string
		dest string
	}{
		{"absolute external host", "https://evil.example.com/course/1"},
		{"http downgrade external", "http://evil.example.com/course/1"},
		{"cloud metadata IP", "http://169.254.169.254/latest/meta-data/"},
		{"scheme switch", "file:///etc/passwd"},
		{"scheme-relative external", "//evil.example.com/course/1"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := client.getCourse(tc.dest)
			if !errors.Is(err, ErrInvalidHost) {
				t.Errorf("getCourse(%q) error = %v, want %v", tc.dest, err, ErrInvalidHost)
			}
		})
	}
}
