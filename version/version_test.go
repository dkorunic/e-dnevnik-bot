package version

import (
	"strings"
	"testing"
)

func TestReadVersion(t *testing.T) {
	t.Parallel()
	// Test with a known dependency
	path := "github.com/dkorunic/e-dnevnik-bot"
	version := ReadVersion(path)

	if !strings.Contains(version, path) {
		t.Errorf("ReadVersion() = %s, want it to contain %s", version, path)
	}
}

func TestReadVersionNotFound(t *testing.T) {
	t.Parallel()
	// A non-existent dependency should return just the path unchanged.
	path := "github.com/non/existent/dependency/xyz"
	version := ReadVersion(path)

	if version != path {
		t.Errorf("ReadVersion() = %s, want %s (just the path when dep not found)", version, path)
	}
}
