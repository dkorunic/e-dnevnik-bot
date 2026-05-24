// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"reflect"
	"testing"
	"time"
)

// TestQueueStoreCtxLiveCtx checks that queueStoreCtx passes a still-live
// context through unchanged so cancellation continues to propagate.
func TestQueueStoreCtxLiveCtx(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sctx, scancel := queueStoreCtx(ctx)
	defer scancel()

	if sctx.Err() != nil {
		t.Fatalf("expected live ctx, got error: %v", sctx.Err())
	}

	cancel()

	// Parent cancel must propagate through the live path.
	if sctx.Err() == nil {
		t.Error("cancelling parent did not propagate to returned ctx")
	}
}

// TestQueueStoreCtxCancelledParent verifies the load-bearing invariant:
// when the parent ctx is already cancelled, queueStoreCtx returns a
// detached short-lived ctx so the post-send queue write still completes.
// A regression that returned the cancelled ctx unchanged would silently
// drop messages on shutdown.
func TestQueueStoreCtxCancelledParent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	sctx, scancel := queueStoreCtx(ctx)
	defer scancel()

	if sctx.Err() != nil {
		t.Fatalf("expected detached live ctx, got Err = %v", sctx.Err())
	}

	deadline, ok := sctx.Deadline()
	if !ok {
		t.Fatal("expected deadline on detached ctx, got none")
	}

	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > storeTimeout {
		t.Errorf("deadline = %v from now, want (0, %v]", remaining, storeTimeout)
	}
}

func TestMergeSkipRecipients(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		existing []string
		extras   []string
		want     []string
	}{
		{
			// Non-empty extras triggers the merge path, which also dedupes existing.
			name:     "duplicates within existing (merge path)",
			existing: []string{"a", "a", "b"},
			extras:   []string{"c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "duplicates within extras",
			existing: []string{"a"},
			extras:   []string{"b", "b", "c"},
			want:     []string{"a", "b", "c"},
		},
		{
			name:     "overlap between slices",
			existing: []string{"a", "b"},
			extras:   []string{"b", "c"},
			want:     []string{"a", "b", "c"},
		},
		{
			// Empty extras short-circuits; callers dedupe before storing.
			name:     "empty extras short-circuits unchanged",
			existing: []string{"a", "b"},
			extras:   nil,
			want:     []string{"a", "b"},
		},
		{
			name:     "both empty",
			existing: nil,
			extras:   nil,
			want:     nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := mergeSkipRecipients(tc.existing, tc.extras)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("mergeSkipRecipients(%v, %v) = %v, want %v",
					tc.existing, tc.extras, got, tc.want)
			}
		})
	}
}

func TestTruncateWithEllipsis(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		input string
		max   int
		want  string
	}{
		{"shorter than max", "hello", 10, "hello"},
		{"exact max length", "hello", 5, "hello"},
		{"longer than max", "hello world", 8, "hello..."},
		{"empty string", "", 5, ""},
		{"unicode multibyte", "héllo wörld", 8, "héllo..."},
		{"exactly max+1", "abcdef", 5, "ab..."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := truncateWithEllipsis(tt.input, tt.max)
			if got != tt.want {
				t.Errorf("truncateWithEllipsis(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
			}
		})
	}
}

// TestTruncateWithEllipsisRuneCount verifies Bug 12A from TESTING-PLAN:
// the result must never exceed max runes. A mutation changing `m-3` to `m-2`
// in the cutoff calculation produces a result that is m+1 runes long.
func TestTruncateWithEllipsisRuneCount(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		input string
		max   int
	}{
		{"ascii", "abcdefghijk", 8},
		{"unicode", "héllo wörld extra", 9},
		{"exactly-max-plus-1", "abcde", 4},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got := truncateWithEllipsis(tc.input, tc.max)
			runeCount := len([]rune(got))

			if runeCount > tc.max {
				t.Errorf("truncateWithEllipsis(%q, %d) = %q has %d runes, exceeds max %d (m-3→m-2 mutation?)",
					tc.input, tc.max, got, runeCount, tc.max)
			}
		})
	}
}

// TestTruncateWithEllipsisMinimumCase verifies Bug 12B from TESTING-PLAN:
// with m=4 and a 5-rune string, the result must be exactly 4 runes
// (1 content rune + "..."). The `>=` mutation would fire one rune too early,
// producing "..." with no content rune (3 runes) for a 4-rune input.
func TestTruncateWithEllipsisMinimumCase(t *testing.T) {
	t.Parallel()

	// 5-rune string, max=4 must yield "a...".
	got := truncateWithEllipsis("abcde", 4)
	runeCount := len([]rune(got))

	if runeCount != 4 {
		t.Errorf("truncateWithEllipsis(%q, 4) = %q has %d runes, want exactly 4", "abcde", got, runeCount)
	}

	if got != "a..." {
		t.Errorf("truncateWithEllipsis(%q, 4) = %q, want %q", "abcde", got, "a...")
	}
}
