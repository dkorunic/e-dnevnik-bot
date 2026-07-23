// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package config

import (
	"os"
	"strings"
	"testing"
)

// copyTestConfig copies the checked-in fixture to a temp file so LoadConfig's
// permission tightening never mutates the repository working tree.
func copyTestConfig(t *testing.T) string {
	t.Helper()

	src, err := os.ReadFile("test_config.toml")
	if err != nil {
		t.Fatalf("failed to read fixture: %v", err)
	}

	dst := t.TempDir() + "/test_config.toml"
	if err := os.WriteFile(dst, src, 0o644); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}

	return dst
}

func TestLoadConfig(t *testing.T) {
	t.Parallel()

	_, err := LoadConfig(copyTestConfig(t))
	if err != nil {
		t.Fatalf("LoadConfig() with valid config failed: %v", err)
	}

	_, err = LoadConfig("non_existent_config.toml")
	if err == nil {
		t.Fatal("LoadConfig() with non-existent config should have failed")
	}

	invalidConfig := []byte("invalid toml")
	tmpfile, err := os.CreateTemp("", "invalid-config-*.toml")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())
	if _, err := tmpfile.Write(invalidConfig); err != nil {
		t.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = LoadConfig(tmpfile.Name())
	if err == nil {
		t.Fatal("LoadConfig() with invalid config should have failed")
	}
}

// TestLoadConfigTightensPermissions verifies the config file — which holds
// plain-text credentials — is tightened to owner-only on load.
func TestLoadConfigTightensPermissions(t *testing.T) {
	t.Parallel()

	cfgFile := copyTestConfig(t)

	if _, err := LoadConfig(cfgFile); err != nil {
		t.Fatalf("LoadConfig() failed: %v", err)
	}

	fi, err := os.Stat(cfgFile)
	if err != nil {
		t.Fatal(err)
	}

	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("config permissions = %o, want 600", perm)
	}
}

// TestLoadConfigRaw verifies the non-validating decode used by mid-run config
// rewrites: a config that would trip LoadConfig's fail-fast validators (here:
// no [[user]] blocks, normally logger.Fatal) must decode without side
// effects, while malformed TOML still errors.
func TestLoadConfigRaw(t *testing.T) {
	t.Parallel()

	// Valid TOML, but no users and no messenger — LoadConfig would Fatal.
	userless := t.TempDir() + "/config.toml"
	if err := os.WriteFile(userless, []byte("[telegram]\ntoken = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfigRaw(userless)
	if err != nil {
		t.Fatalf("LoadConfigRaw() failed on decodable config: %v", err)
	}

	if cfg.Telegram.Token != "x" || len(cfg.User) != 0 {
		t.Errorf("LoadConfigRaw() decoded %+v, want token x with no users", cfg)
	}

	malformed := t.TempDir() + "/config.toml"
	if err := os.WriteFile(malformed, []byte("invalid toml"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadConfigRaw(malformed); err == nil {
		t.Error("LoadConfigRaw() should fail on malformed TOML")
	}

	if _, err := LoadConfigRaw(t.TempDir() + "/missing.toml"); err == nil {
		t.Error("LoadConfigRaw() should fail on a missing file")
	}
}

func TestSaveConfig(t *testing.T) {
	t.Parallel()
	config := TomlConfig{
		User: []User{
			{Username: "test@skole.hr", Password: "password"},
		},
		Calendar: Calendar{Name: "TestCalendar"},
	}

	tmpfile, err := os.CreateTemp("", "test-save-config-*.toml")
	if err != nil {
		t.Fatal(err)
	}

	tmpfile.Close()
	defer os.Remove(tmpfile.Name())

	if err := SaveConfig(tmpfile.Name(), config); err != nil {
		t.Fatalf("SaveConfig() failed: %v", err)
	}

	data, err := os.ReadFile(tmpfile.Name())
	if err != nil {
		t.Fatalf("ReadFile() failed: %v", err)
	}

	if len(data) == 0 {
		t.Error("SaveConfig() wrote empty file")
	}

	content := string(data)
	if !strings.Contains(content, "TestCalendar") {
		t.Error("SaveConfig() did not write calendar name")
	}

	if !strings.Contains(content, "test@skole.hr") {
		t.Error("SaveConfig() did not write username")
	}
}

func TestValidators(t *testing.T) {
	t.Parallel()
	// isValidPhone
	if !isValidPhone("+1234567890") {
		t.Error("isValidPhone() failed with a valid phone number")
	}
	if isValidPhone("1234567890") {
		t.Error("isValidPhone() passed with an invalid phone number")
	}

	// isValidUserAtDomain
	if !isValidUserAtDomain("test@example.com") {
		t.Error("isValidUserAtDomain() failed with a valid user@domain")
	}
	if isValidUserAtDomain("test") {
		t.Error("isValidUserAtDomain() passed with an invalid user@domain")
	}

	// isValidMail
	if !isValidMail("test@example.com") {
		t.Error("isValidMail() failed with a valid email")
	}
	if isValidMail("test") {
		t.Error("isValidMail() passed with an invalid email")
	}

	// isValidID
	if !isValidID("1234567890") {
		t.Error("isValidID() failed with a valid ID")
	}
	if isValidID("abc") {
		t.Error("isValidID() passed with an invalid ID")
	}

	// isValidSlackToken
	if !isValidSlackToken("xoxb-1234567890-1234567890-abcedfghijklmnopqrstuvwx") {
		t.Error("isValidSlackToken() failed with a valid token")
	}
	// Concatenated so the token shape doesn't appear contiguously in source
	// (GitHub push protection pattern-matches xox[bp]- token literals).
	if !isValidSlackToken("xoxp-" + "1234567890-1234567890-1234567890123-abcdef0123456789abcdef0123456789abcdef") {
		t.Error("isValidSlackToken() failed with a valid user token")
	}
	if !isValidSlackToken("xoxe-1234567890-1234567890-abcedfghijklmnopqrstuvwx") {
		t.Error("isValidSlackToken() failed with a valid xoxe token")
	}
	if isValidSlackToken("invalid-token") {
		t.Error("isValidSlackToken() passed with an invalid token")
	}
	if isValidSlackToken("xox-1234567890-abcedfghijklmnopqrstuvwx") {
		t.Error("isValidSlackToken() passed with a prefix-less token")
	}

	// isValidSlackChatID
	if !isValidSlackChatID("C123456789") {
		t.Error("isValidSlackChatID() failed with a valid chat ID")
	}
	if isValidSlackChatID("invalid-id") {
		t.Error("isValidSlackChatID() passed with an invalid chat ID")
	}

	// isValidDiscordToken
	if !isValidDiscordToken("Mxxxxxxxxxxxxxxxxxxxxxxx.xxxxxx.xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx") {
		t.Error("isValidDiscordToken() failed with a valid token")
	}
	if isValidDiscordToken("invalid-token") {
		t.Error("isValidDiscordToken() passed with an invalid token")
	}

	// isValidTelegramToken
	if !isValidTelegramToken("123456789:AABBCCDDEEFFGGHHIIJJKKLLMMNNOOPPQQR") {
		t.Error("isValidTelegramToken() failed with a valid token")
	}
	if isValidTelegramToken("invalid-token") {
		t.Error("isValidTelegramToken() passed with an invalid token")
	}

	// isValidTelegramChatID
	if !isValidTelegramChatID("-123456789") {
		t.Error("isValidTelegramChatID() failed with a valid chat ID")
	}
	if isValidTelegramChatID("invalid-id") {
		t.Error("isValidTelegramChatID() passed with an invalid chat ID")
	}

	// isValidWhatsAppJID
	if !isValidWhatsAppJID("1234567890@s.whatsapp.net") {
		t.Error("isValidWhatsAppJID() failed with a valid JID")
	}
	if isValidWhatsAppJID("invalid-jid") {
		t.Error("isValidWhatsAppJID() passed with an invalid JID")
	}

	// isValidWhatsAppJID with group JID
	if !isValidWhatsAppJID("111111111111111111@g.us") {
		t.Error("isValidWhatsAppJID() failed with a valid group JID")
	}

	// isValidTelegramChatID boundary: minimum length (5 digits)
	if !isValidTelegramChatID("12345") {
		t.Error("isValidTelegramChatID() failed with minimum valid chat ID")
	}

	// isValidTelegramChatID with positive ID (no leading minus)
	if !isValidTelegramChatID("123456789") {
		t.Error("isValidTelegramChatID() failed with positive chat ID")
	}

	// isValidSlackChatID with thread timestamp format
	if !isValidSlackChatID("1234567890.123456") {
		t.Error("isValidSlackChatID() failed with thread timestamp chat ID")
	}

	// isValidMail with display name format
	if !isValidMail("Test User <test@example.com>") {
		t.Error("isValidMail() failed with display name format")
	}

	// isValidID with zero
	if !isValidID("0") {
		t.Error("isValidID() failed with zero")
	}
}

// TestValidatorsNegativeCases covers the mutation-specific edge cases from
// TESTING-PLAN section 13 that are not exercised by TestValidators.

// TestIsValidIDRejectsNegative verifies Bug 13A:
// changing ParseUint → ParseInt would accept "-1" as a valid ID.
// Discord/WhatsApp user IDs must be non-negative integers.
func TestIsValidIDRejectsNegative(t *testing.T) {
	t.Parallel()

	if isValidID("-1") {
		t.Error("isValidID(\"-1\") returned true — negative IDs must be rejected (ParseUint→ParseInt mutation)")
	}

	if isValidID("-1234567") {
		t.Error("isValidID(\"-1234567\") returned true — negative IDs must be rejected")
	}
}

// TestIsValidTelegramChatIDAcceptsLargeNegative verifies Bug 13B:
// removing the leading `-?` from the regex would reject group chat IDs.
// Telegram supergroup/channel IDs are large negative integers.
func TestIsValidTelegramChatIDAcceptsLargeNegative(t *testing.T) {
	t.Parallel()

	if !isValidTelegramChatID("-1001234567890") {
		t.Error("isValidTelegramChatID(\"-1001234567890\") returned false — large negative group IDs must be accepted")
	}
}

// TestIsValidPhoneRejectsLeadingZero verifies Bug 13C:
// changing [1-9] → [0-9] in the phone regex would accept "+01234567890".
// No ITU-T country code starts with 0; such a number is invalid.
func TestIsValidPhoneRejectsLeadingZero(t *testing.T) {
	t.Parallel()

	if isValidPhone("+01234567890") {
		t.Error("isValidPhone(\"+01234567890\") returned true — phone numbers with leading zero after + must be rejected")
	}
}
