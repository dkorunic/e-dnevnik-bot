// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func TestReadVersion(t *testing.T) {
	t.Parallel()

	path := "github.com/dkorunic/e-dnevnik-bot"
	version := ReadVersion(path)

	if !strings.Contains(version, path) {
		t.Errorf("ReadVersion() = %s, want it to contain %s", version, path)
	}
}

func TestReadVersionNotFound(t *testing.T) {
	t.Parallel()
	// Non-existent dependency returns path unchanged.
	path := "github.com/non/existent/dependency/xyz"
	version := ReadVersion(path)

	if version != path {
		t.Errorf("ReadVersion() = %s, want %s (just the path when dep not found)", version, path)
	}
}

// TestReadVersionFormatsMatchedDependency covers the match arm, which no other
// test reaches: a `go test` binary's build info carries an empty Deps list, so
// every lookup falls through to the bare-path return and the "path@version"
// formatting is never exercised. Seeding the package cache directly is the only
// way to test it in-process.
//
// Not parallel: replaces the package-level dependency cache.
func TestReadVersionFormatsMatchedDependency(t *testing.T) {
	// Trip the sync.Once first, so the seeded cache is not overwritten by a
	// lazy read on the next call.
	_ = ReadVersion("trip-the-once")

	orig := deps

	t.Cleanup(func() { deps = orig })

	deps = []*debug.Module{
		{Path: "github.com/rs/zerolog", Version: "v1.34.0"},
		{Path: "github.com/avast/retry-go/v5", Version: "v5.0.1"},
	}

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "first entry matches",
			path: "github.com/rs/zerolog",
			want: "github.com/rs/zerolog@v1.34.0",
		},
		{
			name: "later entry matches",
			path: "github.com/avast/retry-go/v5",
			want: "github.com/avast/retry-go/v5@v5.0.1",
		},
		{
			name: "unmatched path returns unchanged",
			path: "github.com/absent/module",
			want: "github.com/absent/module",
		},
		{
			name: "match is exact, not a prefix",
			path: "github.com/rs",
			want: "github.com/rs",
		},
		{
			name: "empty path returns empty",
			path: "",
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ReadVersion(tt.path); got != tt.want {
				t.Errorf("ReadVersion(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// TestReadVersionIsCached: the build info is read once behind a sync.Once and
// reused, so repeated calls must stay consistent. The messengers call this per
// send to stamp a user agent.
func TestReadVersionIsCached(t *testing.T) {
	t.Parallel()

	const path = "github.com/rs/zerolog"

	first := ReadVersion(path)
	for range 5 {
		if got := ReadVersion(path); got != first {
			t.Fatalf("ReadVersion(%q) returned %q then %q; the cached build info must be stable", path, first, got)
		}
	}
}
