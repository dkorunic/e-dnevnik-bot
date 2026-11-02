// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package messenger

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/queue"
)

// A queue row can wait out a long outage, and a chat messenger would then
// announce an exam already sat or a digest for a week already over. Only what
// has a date to miss expires: a late grade is still news.
func TestResendQueuedDropsExpiredMessages(t *testing.T) {
	t.Parallel()

	eDB := newRunTestDB(t)
	day := 24 * time.Hour
	now := time.Now().UTC()

	for _, m := range []msgtypes.Message{
		{Subject: "past exam", Code: msgtypes.Exam, Timestamp: now.Add(-2 * day)},
		{Subject: "exam today", Code: msgtypes.Exam, Timestamp: now},
		{Subject: "undated exam", Code: msgtypes.Exam},
		{Subject: "digest for last week", Code: msgtypes.ExamDigest, Timestamp: now.Add(-8 * day)},
		{Subject: "digest for this week", Code: msgtypes.ExamDigest, Timestamp: now.Add(-6 * day)},
		{Subject: "old grade", Code: msgtypes.Grade, Timestamp: now.Add(-20 * day)},
	} {
		if err := queue.StoreFailedMsgs(t.Context(), eDB, TelegramQueueName, m); err != nil {
			t.Fatalf("StoreFailedMsgs() = %v", err)
		}
	}

	var seen []string

	resendQueued(t.Context(), eDB, TelegramQueueName, func(m msgtypes.Message) { seen = append(seen, m.Subject) })

	want := []string{"exam today", "undated exam", "digest for this week", "old grade"}
	if !slices.Equal(seen, want) {
		t.Errorf("resent = %v, want %v", seen, want)
	}

	if got := queuedSubjects(t, eDB, TelegramQueueName); len(got) != 0 {
		t.Errorf("queue = %v, want expired rows removed too", got)
	}
}

// The store holds the WhatsApp session, which is enough to act as the device.
// Not parallel: t.Chdir, since the store's path is relative to the working directory.
func TestOpenWhatsAppStoreIsPrivate(t *testing.T) {
	t.Chdir(t.TempDir())

	if err := os.WriteFile(WhatsAppDBName, nil, 0o644); err != nil { //nolint:gosec // the mode under test
		t.Fatalf("WriteFile() = %v", err)
	}

	st, err := OpenWhatsAppStore(context.Background())
	if err != nil {
		t.Fatalf("OpenWhatsAppStore() = %v", err)
	}

	defer st.Close() //nolint:errcheck

	if err := st.Upgrade(context.Background()); err != nil {
		t.Fatalf("Upgrade() = %v", err)
	}

	fi, err := os.Stat(WhatsAppDBName)
	if err != nil {
		t.Fatalf("Stat() = %v", err)
	}

	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("session store mode = %o, want 600", got)
	}
}
