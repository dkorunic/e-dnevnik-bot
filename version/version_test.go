package version

import (
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
