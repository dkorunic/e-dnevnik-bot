// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package version

import (
	"runtime/debug"
	"sync"
)

var (
	depsOnce sync.Once
	deps     []*debug.Module
)

// ReadVersion takes a string path as an argument and returns a string in the format "path@version".
// It does this by reading the build info once (cached) and iterating through the Dependencies slice of
// the BuildInfo struct. If it finds a match for the given path, it will return a string with the path
// and version number joined by an '@' character. If no match is found, it will simply return the path.
func ReadVersion(path string) string {
	depsOnce.Do(func() {
		if i, ok := debug.ReadBuildInfo(); ok {
			deps = i.Deps
		}
	})

	for _, d := range deps {
		if d.Path == path {
			return path + "@" + d.Version
		}
	}

	return path
}
