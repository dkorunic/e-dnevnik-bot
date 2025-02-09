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

package version

import (
	"runtime/debug"
	"strings"
)

// ReadVersion takes a string path as an argument and returns a string in the format "path@version".
// It does this by calling debug.ReadBuildInfo() and then iterating through the Dependencies slice of
// the BuildInfo struct. If it finds a match for the given path, it will return a string with the path
// and version number joined by an '@' character. If no match is found, it will simply return the path.
func ReadVersion(path string) string {
	i, ok := debug.ReadBuildInfo()
	if ok {
		for _, d := range i.Deps {
			if d.Path == path {
				return strings.Join([]string{path, d.Version}, "@")
			}
		}
	}

	return path
}
