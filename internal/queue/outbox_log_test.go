// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package queue

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
	"github.com/rs/zerolog"
)

// Rows in the outbox are not failed messages and nothing resends them from there.
// Not parallel: swaps the global logger.
func TestFetchOutboxDoesNotReportAResend(t *testing.T) {
	eDB, err := sqlitedb.New(t.Context(), filepath.Join(t.TempDir(), "log.db.sqlite"))
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	defer eDB.Close() //nolint:errcheck

	if err := StoreOutbox(t.Context(), eDB, msgtypes.Message{Subject: "s"}); err != nil {
		t.Fatalf("StoreOutbox() = %v", err)
	}

	var buf bytes.Buffer

	orig := logger.Logger
	logger.Logger = zerolog.New(&buf)

	t.Cleanup(func() { logger.Logger = orig })

	if got := FetchFailedMsgs(t.Context(), eDB, OutboxKey); len(got) != 1 {
		t.Fatalf("fetch = %+v, want the row", got)
	}

	if out := buf.String(); strings.Contains(out, "trying to resend") {
		t.Errorf("log = %q", out)
	}
}
