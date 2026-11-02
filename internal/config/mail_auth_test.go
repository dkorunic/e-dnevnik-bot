// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

const mailAuthCaseEnv = "E_DNEVNIK_MAIL_AUTH_CASE"

// The client always uses SMTP AUTH, so a missing login can only fail every send
// and queue alerts until they expire. It has to stop startup instead.
func TestCheckMailConfRequiresLogin(t *testing.T) {
	t.Parallel()

	if v := os.Getenv(mailAuthCaseEnv); v != "" {
		if v == "-" {
			v = ""
		}

		cfg := TomlConfig{Mail: Mail{Server: "smtp.example.com", From: "a@example.com", To: []string{"b@example.com"}, Username: v}}
		checkMailConf(&cfg)
		os.Exit(0)
	}

	cases := map[string]string{"no username": "-", "no password": "user"}

	for name, username := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.Command(os.Args[0], "-test.run=^TestCheckMailConfRequiresLogin$") //nolint:gosec // re-exec of this test binary
			cmd.Env = append(os.Environ(), mailAuthCaseEnv+"="+username)

			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("checkMailConf accepted a config with %v:\n%s", name, out)
			}

			if !strings.Contains(string(out), "mail") {
				t.Errorf("failed for another reason:\n%s", out)
			}
		})
	}
}
