// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package config

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/google/renameio/v2/maybe"
)

// LoadConfig decodes the TOML config at file and runs per-messenger
// validation, enabling each messenger whose block validates. Fatal on an
// invalid block or when no messenger is enabled.
//
// The config holds plain-text credentials, so LoadConfig best-effort tightens
// the file to 0600 — an accidental 0644 must not leave tokens
// group/world-readable.
func LoadConfig(file string) (TomlConfig, error) {
	var config TomlConfig
	if _, err := toml.DecodeFile(file, &config); err != nil {
		return config, fmt.Errorf("failed to parse config file %s: %w", file, err)
	}

	// Best-effort: warn and continue on failure (read-only fs, foreign owner).
	if err := os.Chmod(file, 0o600); err != nil {
		logger.Warn().Msgf("Unable to tighten permissions on config file %q to 0600: %v", file, err)
	}

	checkUserConf(&config)
	checkDiscordConf(&config)
	checkTelegramConf(&config)
	checkSlackConf(&config)
	checkMailConf(&config)
	checkCalendarConf(&config)
	checkWhatsAppConf(&config)

	noMessengerEnabled := !config.WhatsAppEnabled &&
		!config.DiscordEnabled &&
		!config.TelegramEnabled &&
		!config.SlackEnabled &&
		!config.MailEnabled &&
		!config.CalendarEnabled
	if noMessengerEnabled {
		logger.Fatal().Msg("Configuration error: no messenger enabled")
	}

	return config, nil
}

// SaveConfig atomically writes config to file as TOML with 0600 permissions.
func SaveConfig(file string, config TomlConfig) error {
	buf := new(bytes.Buffer)
	if err := toml.NewEncoder(buf).Encode(config); err != nil {
		return err
	}

	if err := maybe.WriteFile(file, buf.Bytes(), 0o600); err != nil {
		return err
	}

	return nil
}

// LoadConfigRaw decodes the TOML config at file WITHOUT validation. It exists
// for in-place rewrites (WhatsApp group resolution, Telegram chat-ID remap)
// that run from messenger goroutines mid-cycle: LoadConfig's fail-fast
// logger.Fatal on a broken config would os.Exit mid-cycle, bypassing graceful
// shutdown and queue writes. Startup must use LoadConfig — never this.
func LoadConfigRaw(file string) (TomlConfig, error) {
	var config TomlConfig
	if _, err := toml.DecodeFile(file, &config); err != nil {
		return config, fmt.Errorf("failed to parse config file %s: %w", file, err)
	}

	return config, nil
}

// checkWhatsAppConf validates the WhatsApp block (phone format, JIDs, group
// names), sorts groups for binary search, and enables the messenger. Fatal on
// any invalid entry.
func checkWhatsAppConf(config *TomlConfig) {
	if len(config.WhatsApp.UserIDs) > 0 || len(config.WhatsApp.Groups) > 0 {
		if config.WhatsApp.PhoneNumber != "" && !isValidPhone(config.WhatsApp.PhoneNumber) {
			logger.Fatal().Msgf("WhatsApp phone number %s is not in international format",
				config.WhatsApp.PhoneNumber)
		}

		for _, u := range config.WhatsApp.UserIDs {
			if !isValidWhatsAppJID(u) {
				logger.Fatal().Msgf("Configuration error: WhatsApp User ID %v is not valid", u)
			}
		}

		for _, g := range config.WhatsApp.Groups {
			if g == "" {
				logger.Fatal().Msg("Configuration error: empty WhatsApp group name")
			}
		}

		// Log after validators so a Fatal leaves the error as the last line.
		logger.Info().Msg("Configuration: Whatsapp messenger enabled (pending check during initialization)")

		// Pre-sort for binary search in messenger.
		slices.SortFunc(config.WhatsApp.Groups, strings.Compare)

		config.WhatsAppEnabled = true
	}
}

// checkCalendarConf enables the Calendar messenger when a calendar name is set.
func checkCalendarConf(config *TomlConfig) {
	if config.Calendar.Name != "" {
		logger.Info().Msg("Configuration: Google Calendar messenger enabled")

		config.CalendarEnabled = true
	}
}

// checkMailConf validates the mail block (recipients, optional From, optional
// port) and enables the messenger. Fatal on any invalid entry.
func checkMailConf(config *TomlConfig) {
	if config.Mail.Server != "" {
		if len(config.Mail.To) == 0 {
			logger.Fatal().Msg("Configuration error: no mail to addresses defined")
		}

		if config.Mail.From != "" && !isValidMail(config.Mail.From) {
			logger.Fatal().Msgf("Configuration error: mail from %v is not in mail format", config.Mail.From)
		}

		// Fail fast instead of silently defaulting to 587; empty = mail.go default.
		if config.Mail.Port != "" {
			portNum, err := strconv.Atoi(config.Mail.Port)
			if err != nil || portNum < 1 || portNum > 65535 {
				logger.Fatal().Msgf("Configuration error: mail port %q is not a valid TCP port (1-65535)", config.Mail.Port)
			}
		}

		for _, t := range config.Mail.To {
			if !isValidMail(t) {
				logger.Fatal().Msgf("Configuration error: mail to %v is not in mail format", t)
			}
		}

		// Log after validators so a Fatal leaves the error as the last line.
		logger.Info().Msg("Configuration: mail messenger enabled")

		config.MailEnabled = true
	}
}

// checkSlackConf validates the Slack block (token and chat IDs) and enables the
// messenger. Fatal on any invalid entry.
func checkSlackConf(config *TomlConfig) {
	if config.Slack.Token != "" {
		if !isValidSlackToken(config.Slack.Token) {
			logger.Fatal().Msgf("Configuration error: Slack token %v... is not valid", config.Slack.Token[:min(8, len(config.Slack.Token))])
		}

		if len(config.Slack.ChatIDs) == 0 {
			logger.Fatal().Msg("Configuration error: Slack chat IDs not defined")
		}

		for _, c := range config.Slack.ChatIDs {
			if !isValidSlackChatID(c) {
				logger.Fatal().Msgf("Configuration error: Slack chat ID %v is not valid", c)
			}
		}

		logger.Info().Msg("Configuration: Slack messenger enabled")

		config.SlackEnabled = true
	}
}

// checkTelegramConf validates the Telegram block (token and chat IDs) and
// enables the messenger. Fatal on any invalid entry.
func checkTelegramConf(config *TomlConfig) {
	if config.Telegram.Token != "" {
		if !isValidTelegramToken(config.Telegram.Token) {
			logger.Fatal().Msgf("Configuration error: Telegram token %v... is not valid", config.Telegram.Token[:min(8, len(config.Telegram.Token))])
		}

		if len(config.Telegram.ChatIDs) == 0 {
			logger.Fatal().Msg("Configuration error: Telegram chat IDs not defined")
		}

		for _, c := range config.Telegram.ChatIDs {
			if !isValidTelegramChatID(c) {
				logger.Fatal().Msgf("Configuration error: Telegram chat ID %v is not valid", c)
			}
		}

		logger.Info().Msg("Configuration: Telegram messenger enabled")

		config.TelegramEnabled = true
	}
}

// checkDiscordConf validates the Discord block (token and user IDs) and enables
// the messenger. Fatal on any invalid entry.
func checkDiscordConf(config *TomlConfig) {
	if config.Discord.Token != "" {
		if !isValidDiscordToken(config.Discord.Token) {
			logger.Fatal().Msgf("Configuration error: Discord token %v... is not valid", config.Discord.Token[:min(8, len(config.Discord.Token))])
		}

		if len(config.Discord.UserIDs) == 0 {
			logger.Fatal().Msg("Configuration error: Discord User IDs not defined")
		}

		for _, u := range config.Discord.UserIDs {
			if !isValidID(u) {
				logger.Fatal().Msgf("Configuration error: Discord User ID %v is not valid", u)
			}
		}

		logger.Info().Msg("Configuration: Discord messenger enabled")

		config.DiscordEnabled = true
	}
}

// checkUserConf validates the [[user]] blocks: at least one entry, each with
// username+password in User@domain form, no duplicates. Fatal on violation; a
// non-@skole.hr domain only warns.
func checkUserConf(config *TomlConfig) {
	if len(config.User) == 0 {
		logger.Fatal().Msg("Configuration error: No users defined")
	}

	// Dupes cause redundant logins; key on Username only.
	seen := make(map[string]struct{}, len(config.User))

	for _, u := range config.User {
		if u.Username == "" || u.Password == "" {
			logger.Fatal().Msgf("Configuration error: user entry requires both username and password (username: %q)",
				u.Username)
		}

		if !isValidUserAtDomain(u.Username) {
			logger.Fatal().Msgf("Configuration error: username not in proper User@domain format: %v", u.Username)
		}

		if !strings.HasSuffix(u.Username, "@skole.hr") {
			logger.Warn().Msgf("Configuration issue: username not ending with @skole.hr: %v", u.Username)
		}

		if _, dup := seen[u.Username]; dup {
			logger.Fatal().Msgf("Configuration error: duplicate [[user]] block for username %v", u.Username)
		}

		seen[u.Username] = struct{}{}
	}
}
