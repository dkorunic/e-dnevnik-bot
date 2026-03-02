// @license
// Copyright (C) 2025 Dinko Korunic
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package config

import (
	"os"
	"strings"
	"testing"
)

func TestLoadConfig(t *testing.T) {
	t.Parallel()
	// Test with a valid config file.
	_, err := LoadConfig("test_config.toml")
	if err != nil {
		t.Fatalf("LoadConfig() with valid config failed: %v", err)
	}

	// Test with a non-existent config file.
	_, err = LoadConfig("non_existent_config.toml")
	if err == nil {
		t.Fatal("LoadConfig() with non-existent config should have failed")
	}

	// Test with an invalid config file.
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

	// Verify key fields are present in the output.
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
	if isValidSlackToken("invalid-token") {
		t.Error("isValidSlackToken() passed with an invalid token")
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
