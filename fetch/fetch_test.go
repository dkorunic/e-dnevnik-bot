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
	"reflect"
	"testing"
	"time"

	"github.com/jordic/goics"
)

func TestParseFirstDateTime(t *testing.T) {
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
