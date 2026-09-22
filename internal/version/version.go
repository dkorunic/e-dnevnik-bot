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

// ReadVersion returns "path@version" from the cached build info, or just path
// when the dependency is absent.
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
