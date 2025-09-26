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
