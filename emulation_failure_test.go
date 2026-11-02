// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/config"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/rs/zerolog"
)

// -t exists to check credentials, so a send that fails must not end in "success"
// or exit 0. Mail at a closed port is what the real binary got wrong.
// Not parallel: swaps the global logger, flag pointers and the exit latch.
func TestEmulationReportsFailedSend(t *testing.T) {
	resetExitLatch(t)
	setRetries(t, 1)
	setEmulationDB(t, "emulation.db")

	var buf bytes.Buffer

	orig := logger.Logger
	logger.Logger = zerolog.New(&buf)

	t.Cleanup(func() { logger.Logger = orig })

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v", err)
	}

	port := ln.Addr().(*net.TCPAddr).Port //nolint:forcetypeassert // tcp listener

	_ = ln.Close()

	cfg := config.TomlConfig{MailEnabled: true, Mail: config.Mail{
		Server: "127.0.0.1", Port: strconv.Itoa(port), Username: "u", Password: "p",
		From: "a@example.com", To: []string{"b@example.com"},
	}}

	runWithinTimeout(t, 60*time.Second, "testSingleRun", func() { testSingleRun(t.Context(), cfg) })

	if !exitWithError.Load() {
		t.Error("a failed test send did not latch exitWithError, so -t exits 0")
	}

	if strings.Contains(buf.String(), "success") {
		t.Errorf("log claims success after a failed send:\n%s", buf.String())
	}
}
