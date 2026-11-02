// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() = %v", err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() = %v", err)
	}

	return string(b)
}

// Recent session writes live in the -wal until a checkpoint, so it has to move
// with the database.
func TestMigrateWhatsAppDBMovesSidecars(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	oldDB, newDB := filepath.Join(dir, "old.sqlite"), filepath.Join(dir, "new.sqlite")

	writeFile(t, oldDB, "db")
	writeFile(t, oldDB+"-wal", "wal")
	writeFile(t, oldDB+"-shm", "shm")

	migrateWhatsAppDB(oldDB, newDB)

	for suffix, want := range map[string]string{"": "db", "-wal": "wal", "-shm": "shm"} {
		if got := readFile(t, newDB+suffix); got != want {
			t.Errorf("new%v = %q, want %q", suffix, got, want)
		}
	}

	if _, err := os.Stat(oldDB); err == nil {
		t.Error("the old database is still there")
	}
}

// A stray old file must not replace the session in use.
func TestMigrateWhatsAppDBKeepsCurrentSession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	oldDB, newDB := filepath.Join(dir, "old.sqlite"), filepath.Join(dir, "new.sqlite")

	writeFile(t, oldDB, "stale")
	writeFile(t, newDB, "current")

	migrateWhatsAppDB(oldDB, newDB)

	if got := readFile(t, newDB); got != "current" {
		t.Errorf("session store = %q, want it untouched", got)
	}
}
