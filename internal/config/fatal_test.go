// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package config

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// Valid fixtures. Each fatal case below starts from a block that would pass
// validation and breaks exactly one field, so a test failure points at the one
// check that stopped working rather than at the block as a whole.
const (
	validSlackToken    = "xoxb-1234567890-1234567890-abcedfghijklmnopqrstuvwx"
	validSlackChatID   = "C012345678"
	validTelegramToken = "123456789:AABBCCDDEEFFGGHHIIJJKKLLMMNNOOPPQQR"
	validTelegramChat  = "123456789"
	validDiscordToken  = "Mxxxxxxxxxxxxxxxxxxxxxxx.xxxxxx.xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
	validDiscordUserID = "123456789012345678"
	validJID           = "385991234567@s.whatsapp.net"
	validUsername      = "pero.peric@skole.hr"
)

// fatalCaseEnv names the fatal case a re-executed child process should run.
const fatalCaseEnv = "EDNEVNIK_CONFIG_FATAL_CASE"

// fatalCases are configurations that must abort the process at load time.
// Validation is fail-fast by design: a bad token or malformed recipient should
// stop the bot at startup with one clear message, not surface hours later as a
// failed send on the first poll cycle.
var fatalCases = []struct {
	name string
	run  func()
}{
	{
		name: "mail server without recipients",
		run:  func() { checkMailConf(&TomlConfig{Mail: Mail{Server: "smtp.example.com"}}) },
	},
	{
		name: "mail from address is not an address",
		run: func() {
			checkMailConf(&TomlConfig{Mail: Mail{
				Server: "smtp.example.com",
				From:   "not-an-email",
				To:     []string{"pero@example.com"},
			}})
		},
	},
	{
		name: "mail recipient is not an address",
		run: func() {
			checkMailConf(&TomlConfig{Mail: Mail{
				Server: "smtp.example.com",
				To:     []string{"pero@example.com", "bogus"},
			}})
		},
	},
	{
		name: "mail port is not numeric",
		run:  func() { checkMailPort("smtp") },
	},
	{
		name: "mail port is zero",
		run:  func() { checkMailPort("0") },
	},
	{
		name: "mail port is above the TCP range",
		run:  func() { checkMailPort("65536") },
	},
	{
		name: "mail port is negative",
		run:  func() { checkMailPort("-1") },
	},
	{
		name: "slack token is malformed",
		run: func() {
			checkSlackConf(&TomlConfig{Slack: Slack{Token: "not-a-slack-token", ChatIDs: []string{validSlackChatID}}})
		},
	},
	{
		name: "slack token without chat IDs",
		run:  func() { checkSlackConf(&TomlConfig{Slack: Slack{Token: validSlackToken}}) },
	},
	{
		name: "slack chat ID is malformed",
		run: func() {
			checkSlackConf(&TomlConfig{Slack: Slack{Token: validSlackToken, ChatIDs: []string{"nope"}}})
		},
	},
	{
		name: "telegram token is malformed",
		run: func() {
			checkTelegramConf(&TomlConfig{Telegram: Telegram{Token: "123:abc", ChatIDs: []string{validTelegramChat}}})
		},
	},
	{
		name: "telegram token without chat IDs",
		run:  func() { checkTelegramConf(&TomlConfig{Telegram: Telegram{Token: validTelegramToken}}) },
	},
	{
		name: "telegram chat ID is malformed",
		run: func() {
			checkTelegramConf(&TomlConfig{Telegram: Telegram{Token: validTelegramToken, ChatIDs: []string{"abc"}}})
		},
	},
	{
		name: "discord token is malformed",
		run: func() {
			checkDiscordConf(&TomlConfig{Discord: Discord{Token: "bad", UserIDs: []string{validDiscordUserID}}})
		},
	},
	{
		name: "discord token without user IDs",
		run:  func() { checkDiscordConf(&TomlConfig{Discord: Discord{Token: validDiscordToken}}) },
	},
	{
		name: "discord user ID is not numeric",
		run: func() {
			checkDiscordConf(&TomlConfig{Discord: Discord{Token: validDiscordToken, UserIDs: []string{"not-an-id"}}})
		},
	},
	{
		name: "whatsapp phone is not international format",
		run: func() {
			checkWhatsAppConf(&TomlConfig{WhatsApp: WhatsApp{
				PhoneNumber: "0991234567",
				UserIDs:     []string{validJID},
			}})
		},
	},
	{
		name: "whatsapp user ID is not a JID",
		run:  func() { checkWhatsAppConf(&TomlConfig{WhatsApp: WhatsApp{UserIDs: []string{"385991234567"}}}) },
	},
	{
		name: "whatsapp broadcast JID is rejected",
		run: func() {
			checkWhatsAppConf(&TomlConfig{WhatsApp: WhatsApp{UserIDs: []string{"385991234567@broadcast"}}})
		},
	},
	{
		name: "whatsapp group name is empty",
		run:  func() { checkWhatsAppConf(&TomlConfig{WhatsApp: WhatsApp{Groups: []string{""}}}) },
	},
	{
		name: "no users defined",
		run:  func() { checkUserConf(&TomlConfig{}) },
	},
	{
		name: "user without a password",
		run:  func() { checkUserConf(&TomlConfig{User: []User{{Username: validUsername}}}) },
	},
	{
		name: "user without a username",
		run:  func() { checkUserConf(&TomlConfig{User: []User{{Password: "secret"}}}) },
	},
	{
		name: "username is not User@domain",
		run:  func() { checkUserConf(&TomlConfig{User: []User{{Username: "pero", Password: "secret"}}}) },
	},
	{
		name: "duplicate usernames",
		run: func() {
			checkUserConf(&TomlConfig{User: []User{
				{Username: validUsername, Password: "a"},
				{Username: validUsername, Password: "b"},
			}})
		},
	},
}

// TestConfigFatalCases covers the fail-fast validators. They abort via
// logger.Fatal, which bottoms out in os.Exit and cannot be intercepted
// in-process, so each case runs in a re-executed copy of this test binary and
// is judged by its exit status.
func TestConfigFatalCases(t *testing.T) {
	// Child mode: run the one requested case. Reaching the end means the
	// validator did *not* abort, so exit 0 and let the parent fail the case.
	if name := os.Getenv(fatalCaseEnv); name != "" {
		for _, c := range fatalCases {
			if c.name == name {
				c.run()

				os.Exit(0)
			}
		}

		// Unknown case name: exit 0 so the parent reports it as "did not abort"
		// rather than silently passing on an unrelated failure.
		os.Exit(0)
	}

	for _, c := range fatalCases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.Command(os.Args[0], "-test.run=TestConfigFatalCases", "-test.v=false") //nolint:gosec // re-exec of this test binary
			cmd.Env = append(os.Environ(), fatalCaseEnv+"="+c.name)

			out, err := cmd.CombinedOutput()

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("configuration was accepted (child exited %v); this config must be rejected at load time, not on the first send\nchild output:\n%s",
					err, out)
			}

			if exitErr.ExitCode() == 0 {
				t.Fatalf("child exited 0; the validator did not abort\nchild output:\n%s", out)
			}
		})
	}
}

// TestConfigValidBlocksAreAccepted is the complement: each block that the fatal
// cases mutate must pass in its unmutated form, and must set its Enabled flag.
// Without this, a validator that rejected everything would satisfy every fatal
// case above while making the bot unusable.
func TestConfigValidBlocksAreAccepted(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     TomlConfig
		check   func(*TomlConfig)
		enabled func(TomlConfig) bool
	}{
		{
			name: "mail",
			cfg: TomlConfig{Mail: Mail{
				Server: "smtp.example.com",
				Port:   "587",
				From:   "bot@example.com",
				To:     []string{"pero@example.com", "ana@example.com"},
			}},
			check:   checkMailConf,
			enabled: func(c TomlConfig) bool { return c.MailEnabled },
		},
		{
			name:    "slack",
			cfg:     TomlConfig{Slack: Slack{Token: validSlackToken, ChatIDs: []string{validSlackChatID}}},
			check:   checkSlackConf,
			enabled: func(c TomlConfig) bool { return c.SlackEnabled },
		},
		{
			name:    "telegram",
			cfg:     TomlConfig{Telegram: Telegram{Token: validTelegramToken, ChatIDs: []string{validTelegramChat}}},
			check:   checkTelegramConf,
			enabled: func(c TomlConfig) bool { return c.TelegramEnabled },
		},
		{
			name:    "discord",
			cfg:     TomlConfig{Discord: Discord{Token: validDiscordToken, UserIDs: []string{validDiscordUserID}}},
			check:   checkDiscordConf,
			enabled: func(c TomlConfig) bool { return c.DiscordEnabled },
		},
		{
			name: "whatsapp",
			cfg: TomlConfig{WhatsApp: WhatsApp{
				PhoneNumber: "+385991234567",
				UserIDs:     []string{validJID},
				Groups:      []string{"Razred 5.a"},
			}},
			check:   checkWhatsAppConf,
			enabled: func(c TomlConfig) bool { return c.WhatsAppEnabled },
		},
		{
			name:    "calendar",
			cfg:     TomlConfig{Calendar: Calendar{Name: "e-Dnevnik"}},
			check:   checkCalendarConf,
			enabled: func(c TomlConfig) bool { return c.CalendarEnabled },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := tt.cfg
			tt.check(&cfg)

			if !tt.enabled(cfg) {
				t.Errorf("a valid %v block did not enable the messenger", tt.name)
			}
		})
	}
}

// TestCheckConfLeavesEmptyBlocksDisabled: every messenger section is
// independently optional, and absence must disable it silently rather than
// error. A user with only Discord configured must not be told about Slack.
func TestCheckConfLeavesEmptyBlocksDisabled(t *testing.T) {
	t.Parallel()

	var cfg TomlConfig

	checkMailConf(&cfg)
	checkSlackConf(&cfg)
	checkTelegramConf(&cfg)
	checkDiscordConf(&cfg)
	checkWhatsAppConf(&cfg)
	checkCalendarConf(&cfg)

	if cfg.MailEnabled || cfg.SlackEnabled || cfg.TelegramEnabled ||
		cfg.DiscordEnabled || cfg.WhatsAppEnabled || cfg.CalendarEnabled {
		t.Errorf("an empty configuration enabled a messenger: %+v", cfg)
	}
}

// TestCheckMailPortAcceptsValidPorts: an empty port defers to mail.go's own 587
// default, and the full TCP range must be accepted. A too-strict check here
// would abort startup for a legitimate submission or SMTPS port.
func TestCheckMailPortAcceptsValidPorts(t *testing.T) {
	t.Parallel()

	for _, port := range []string{"", "1", "25", "465", "587", "2525", "65535"} {
		// Reaching the next statement means no Fatal fired.
		checkMailPort(port)
	}
}

// TestCheckWhatsAppConfSortsGroups: the messenger binary-searches this slice
// when matching joined groups by name, so load-time sorting is a precondition
// for correct matching, not a cosmetic detail.
func TestCheckWhatsAppConfSortsGroups(t *testing.T) {
	t.Parallel()

	cfg := TomlConfig{WhatsApp: WhatsApp{
		UserIDs: []string{validJID},
		Groups:  []string{"Zbor", "Razred 5.a", "Aktiv"},
	}}

	checkWhatsAppConf(&cfg)

	want := []string{"Aktiv", "Razred 5.a", "Zbor"}
	for i, g := range want {
		if cfg.WhatsApp.Groups[i] != g {
			t.Fatalf("Groups = %v, want them sorted as %v — the messenger binary-searches this slice",
				cfg.WhatsApp.Groups, want)
		}
	}
}
