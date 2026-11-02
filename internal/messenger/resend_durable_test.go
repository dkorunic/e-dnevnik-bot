// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"database/sql"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// A requeue that fails while the delete succeeds lost the alert: the original
// row may only go once its replacement is durable. The trigger fails inserts
// alone, as a full disk or a lock timeout can.
func TestResendQueuedKeepsRowWhenRequeueFails(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "resend.db.sqlite")

	eDB, err := sqlitedb.New(t.Context(), path)
	if err != nil {
		t.Fatalf("sqlitedb.New() = %v", err)
	}

	t.Cleanup(func() { _ = eDB.Close() })

	seedQueue(t, eDB, MailQueueName, "only")

	admin, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("sql.Open() = %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if _, err := admin.Exec(`CREATE TRIGGER no_insert BEFORE INSERT ON kv BEGIN SELECT RAISE(ABORT, 'full'); END`); err != nil {
		t.Fatalf("CREATE TRIGGER = %v", err)
	}

	resendQueued(t.Context(), eDB, MailQueueName, func(m msgtypes.Message) {
		storeQueued(t.Context(), eDB, MailQueueName, m)
	})

	if _, err := admin.Exec(`DROP TRIGGER no_insert`); err != nil {
		t.Fatalf("DROP TRIGGER = %v", err)
	}

	if got := queuedSubjects(t, eDB, MailQueueName); !slices.Equal(got, []string{"only"}) {
		t.Errorf("queue after a failed requeue = %v, want the original row kept", got)
	}
}
