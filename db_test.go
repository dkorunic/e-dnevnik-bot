// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/msgtypes"
	"github.com/dkorunic/e-dnevnik-bot/internal/sqlitedb"
)

// mustOpenDB opens the database or fails the test: an open failure here is
// setup breakage, not the behaviour under test.
func mustOpenDB(t *testing.T, ctx context.Context, file string) *sqlitedb.Edb { //nolint:revive // ctx after t is the testing convention here
	t.Helper()

	eDB, err := openDB(ctx, file)
	if err != nil {
		t.Fatalf("openDB(%q) = %v", file, err)
	}

	return eDB
}

// TestOpenCloseDB covers the success path of the two database lifecycle
// helpers. Their failure paths call logger.Fatal (os.Exit) and are therefore
// out of reach of an in-process test.
func TestOpenCloseDB(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "app.db.sqlite")

	eDB := mustOpenDB(t, t.Context(), path)
	if eDB == nil {
		t.Fatal("openDB() returned nil")
	}

	// A brand-new database must report itself as new, which is what suppresses
	// the first-run alert flood.
	if eDB.Existing() {
		t.Error("openDB() on a fresh path reported an existing database; first-run seeding would be skipped and the user flooded")
	}

	if _, err := eDB.CheckAndFlagTTL(t.Context(), "u", "s", []string{"x"}); err != nil {
		t.Fatalf("database is not usable after openDB(): %v", err)
	}

	closeDB(eDB)

	if _, err := os.Stat(path); err != nil {
		t.Errorf("database file missing after closeDB(): %v", err)
	}
}

// TestOpenDBReopenIsExisting pins the first-run detection across restarts: a
// database that already holds rows must report Existing() on reopen, or every
// restart would silently re-seed and swallow that cycle's alerts.
func TestOpenDBReopenIsExisting(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "app.db.sqlite")

	eDB := mustOpenDB(t, t.Context(), path)
	if _, err := eDB.CheckAndFlagTTL(t.Context(), "u", "s", []string{"seed"}); err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}

	closeDB(eDB)

	eDB = mustOpenDB(t, t.Context(), path)
	defer closeDB(eDB)

	if !eDB.Existing() {
		t.Error("a reopened, populated database reported itself as new; every restart would suppress that cycle's alerts")
	}

	// The flag written before the restart must still suppress a duplicate.
	found, err := eDB.CheckAndFlagTTL(t.Context(), "u", "s", []string{"seed"})
	if err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}

	if !found {
		t.Error("a hash flagged before the restart was not found afterwards; alerts would repeat every run")
	}
}

// TestOpenDBDedupSurvivesAcrossHelpers is an end-to-end check of the dedup
// contract the helpers exist to serve: distinct events are independent, and an
// identical event is suppressed on the second sighting.
func TestOpenDBDedupSurvivesAcrossHelpers(t *testing.T) {
	t.Parallel()

	eDB := mustOpenDB(t, t.Context(), filepath.Join(t.TempDir(), "dedup.db.sqlite"))
	defer closeDB(eDB)

	msgs := []msgtypes.Message{
		{Code: msgtypes.Grade, Username: "u", Subject: "Matematika", Fields: []string{"1.9.", "5"}},
		{Code: msgtypes.Grade, Username: "u", Subject: "Matematika", Fields: []string{"1.9.", "4"}},
		{Code: msgtypes.Grade, Username: "u", Subject: "Fizika", Fields: []string{"1.9.", "5"}},
	}

	for i, m := range msgs {
		found, err := eDB.CheckAndFlagTTL(t.Context(), m.Username, m.Subject, m.Fields)
		if err != nil {
			t.Fatalf("message %d: CheckAndFlagTTL() failed: %v", i, err)
		}

		if found {
			t.Errorf("message %d was reported as already seen; distinct events must not collide", i)
		}
	}

	found, err := eDB.CheckAndFlagTTL(t.Context(), msgs[0].Username, msgs[0].Subject, msgs[0].Fields)
	if err != nil {
		t.Fatalf("CheckAndFlagTTL() failed: %v", err)
	}

	if !found {
		t.Error("a repeated event was not detected as a duplicate; the alert would fire twice")
	}
}

// failingCloser drives closeDB's error path; see dbCloser.
type failingCloser struct{}

func (failingCloser) Close() error { return errors.New("close failed") }

// TestCloseDBSurvivesCloseFailure: every write is already committed by the time
// the pool closes and the next cycle opens a fresh handle, so a close error is
// no reason to kill a daemon mid-flight — and os.Exit here would skip the pprof
// flush and main's deferred shutdown. The run must still be flagged failed.
//
// Re-executed: the failure path used to exit the process, which no in-process
// assertion can observe.
func TestCloseDBSurvivesCloseFailure(t *testing.T) {
	if os.Getenv(closeDBCaseEnv) != "" {
		closeDB(failingCloser{})

		// Survived, but a silent survival would hide the failure entirely.
		if !exitWithError.Load() {
			os.Exit(3)
		}

		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestCloseDBSurvivesCloseFailure") //nolint:gosec // re-exec of this test binary
	cmd.Env = append(os.Environ(), closeDBCaseEnv+"=1")

	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 3 {
		t.Fatalf("closeDB swallowed a close failure without flagging the run\noutput:\n%s", out)
	}

	t.Fatalf("closeDB exited the process on a close failure (%v); a daemon must keep polling\noutput:\n%s", err, out)
}

// closeDBCaseEnv marks the re-executed child of TestCloseDBSurvivesCloseFailure.
const closeDBCaseEnv = "EDNEVNIK_CLOSEDB_FAILURE"

// TestCloseDBCleanCloseDoesNotFlag: the latch is process-lifetime, so a healthy
// close must leave it alone or every run would report itself failed.
func TestCloseDBCleanCloseDoesNotFlag(t *testing.T) {
	resetExitLatch(t)

	eDB := mustOpenDB(t, t.Context(), t.TempDir()+"/closedb-clean.db")

	closeDB(eDB)

	if exitWithError.Load() {
		t.Error("a clean close latched the run as failed")
	}
}

// TestOpenDBReturnsErrorInsteadOfExiting: openDB runs once per poll cycle, so a
// Fatal here kills a daemon over a transient failure — and, being os.Exit,
// takes the pprof flush and main's deferred shutdown with it. A directory in
// place of the database file is a durable stand-in for that failure.
//
// Re-executed: an exiting openDB cannot be observed in-process.
func TestOpenDBReturnsErrorInsteadOfExiting(t *testing.T) {
	if dir := os.Getenv(openDBCaseEnv); dir != "" {
		// sqlitedb.New appends ".sqlite"; a directory at that path cannot be
		// opened as a database.
		blocked := filepath.Join(dir, "blocked.db")
		if err := os.MkdirAll(blocked+".sqlite", 0o700); err != nil {
			os.Exit(4)
		}

		eDB, err := openDB(context.Background(), blocked)
		if err == nil {
			_ = eDB.Close()

			os.Exit(3) // opened something it should not have
		}

		os.Exit(0)
	}

	cmd := exec.Command(os.Args[0], "-test.run=TestOpenDBReturnsErrorInsteadOfExiting") //nolint:gosec // re-exec of this test binary
	cmd.Env = append(os.Environ(), openDBCaseEnv+"="+t.TempDir())

	out, err := cmd.CombinedOutput()
	if err == nil {
		return
	}

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		switch exitErr.ExitCode() {
		case 3:
			t.Fatalf("openDB opened a directory as a database\noutput:\n%s", out)
		case 4:
			t.Fatalf("test setup failed\noutput:\n%s", out)
		}
	}

	t.Fatalf("openDB exited the process on a failed open (%v); a daemon must skip the cycle instead\noutput:\n%s", err, out)
}

// openDBCaseEnv marks the re-executed child of TestOpenDBReturnsErrorInsteadOfExiting.
const openDBCaseEnv = "EDNEVNIK_OPENDB_FAILURE"

// TestRunPollCycleSkipsCycleOnUnopenableDB: a cycle that cannot open the
// database has nothing to dedup against, so it must be skipped and flagged
// rather than run against a nil handle.
func TestRunPollCycleSkipsCycleOnUnopenableDB(t *testing.T) {
	resetExitLatch(t)

	blocked := filepath.Join(t.TempDir(), "blocked.db")
	if err := os.MkdirAll(blocked+".sqlite", 0o700); err != nil {
		t.Fatal(err)
	}

	orig := dbFile
	dbFile = &blocked

	t.Cleanup(func() { dbFile = orig })

	runPollCycle(t.Context(), config.TomlConfig{})

	if !exitWithError.Load() {
		t.Error("a cycle skipped for an unopenable database did not flag the run as failed")
	}
}
