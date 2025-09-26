package queue

import (
	"os"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/db"
	"github.com/dkorunic/e-dnevnik-bot/encdec"
	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
)

func TestFetchFailedMsgs(t *testing.T) {
	t.Parallel()
	// Create a temporary database for testing.
	tmpdir, err := os.MkdirTemp("", "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	eDB, err := db.New(tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	queueKey := []byte("test-queue")
	msgs := []msgtypes.Message{
		{Username: "testuser", Subject: "Test Subject"},
	}
	encodedMsgs, _ := encdec.EncodeMsgs(msgs)

	// Store some messages in the queue
	eDB.FetchAndStore(queueKey, func(old []byte) ([]byte, error) {
		return encodedMsgs, nil
	})

	// Fetch the messages
	failedMsgs := FetchFailedMsgs(eDB, queueKey)
	if len(failedMsgs) != 1 {
		t.Fatalf("FetchFailedMsgs() len = %d, want 1", len(failedMsgs))
	}

	if failedMsgs[0].Username != "testuser" {
		t.Errorf("failedMsgs[0].Username = %s, want testuser", failedMsgs[0].Username)
	}
}
