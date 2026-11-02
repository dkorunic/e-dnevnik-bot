// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package sqlitedb

import (
	"bytes"
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/rs/zerolog"
)

// PrepareFile runs once per poll cycle, and what it cannot fix stays unfixed.
// A directory stands in for a file that cannot be opened.
// Not parallel: swaps the global logger.
func TestPrepareFileWarnsOnce(t *testing.T) {
	var buf bytes.Buffer

	orig := logger.Logger
	logger.Logger = zerolog.New(&buf)

	t.Cleanup(func() { logger.Logger = orig })

	dir := t.TempDir()

	PrepareFile(dir)
	PrepareFile(dir)

	if n := strings.Count(buf.String(), "Unable to"); n != 1 {
		t.Errorf("%d warnings, want 1:\n%s", n, buf.String())
	}
}
