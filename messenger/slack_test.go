package messenger

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/sqlitedb"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/socketmode"
	"go.uber.org/ratelimit"
)

func TestProcessSlack(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	api := slack.New("test-token", slack.OptionAPIURL(server.URL+"/"))
	slackCli = socketmode.New(api)

	msg := msgtypes.Message{
		Username:     "testuser",
		Subject:      "Test Subject",
		Descriptions: []string{"desc1"},
		Fields:       []string{"field1"},
	}

	rl := ratelimit.New(1)

	// Create a temporary database for testing.
	tmpdir, err := os.MkdirTemp("", "test.db")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpdir)

	eDB, err := sqlitedb.New(tmpdir)
	if err != nil {
		t.Fatal(err)
	}
	defer eDB.Close()

	processSlack(context.Background(), eDB, msg, []string{"C12345"}, rl, 1)
}

func TestSlackEventHandler(t *testing.T) {
	t.Parallel()
	evt := &socketmode.Event{
		Type: socketmode.EventTypeConnectionError,
		Data: "test error",
	}
	slackEventHandler(evt, nil)
}
