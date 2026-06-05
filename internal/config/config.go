// SPDX-FileCopyrightText: 2022 Dinko Korunic
// SPDX-License-Identifier: MIT

package config

import (
	"bytes"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
	"github.com/google/renameio/v2/maybe"
)

// LoadConfig attempts to load and decode configuration file in TOML format, doing a minimal sanity checking and
// optionally returning an error.
func LoadConfig(file string) (TomlConfig, error) {
	var config TomlConfig
	if _, err := toml.DecodeFile(file, &config); err != nil {
		return config, fmt.Errorf("failed to parse config file %s: %w", file, err)
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

// SaveConfig saves the provided configuration to the specified file in TOML format.
//
// Parameters:
// - file: a string representing the path to save the configuration.
// - config: a TomlConfig struct containing the configuration settings.
//
// Returns:
// - error: an error indicating any issues encountered during the saving process.
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

// checkWhatsAppConf does a minimal sanity check on the WhatsApp configuration block, ensuring that:
//
// 1. the phone number is in international format (if specified)
//
// 2. all User IDs are valid
//
// If any of these conditions are not met, the program will log a fatal error and exit.
//
// If all conditions are met, the program will log an info message about WhatsApp messenger being enabled and will set
// the whatsAppEnabled field of the TomlConfig object to true.
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

// checkCalendarConf does a minimal sanity check on the Google Calendar configuration block, ensuring that:
//
// 1. the Calendar name is not empty
//
// If all conditions are met, the program will log an info message about Google Calendar integration being enabled and
// will set the calendarEnabled field of the TomlConfig object to true.
func checkCalendarConf(config *TomlConfig) {
	if config.Calendar.Name != "" {
		logger.Info().Msg("Configuration: Google Calendar messenger enabled")

		config.CalendarEnabled = true
	}
}

// checkMailConf does a minimal sanity check on the mail configuration block, ensuring that:
//
// 1. the mail server is not empty
//
// 2. all destination mail addresses (TO) are valid
//
// If all conditions are met, the program will log an info message about mail integration being enabled and will set the
// mailEnabled field of the TomlConfig object to true.
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

// checkSlackConf does a minimal sanity check on the Slack configuration block, ensuring that:
//
// 1. the Slack token is defined and valid
//
// 2. the chat IDs are defined and all valid
//
// If all conditions are met, the program will log an info message about Slack integration being enabled and will set the
// slackEnabled field of the TomlConfig object to true.
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

// checkTelegramConf performs a minimal sanity check on the Telegram configuration block, ensuring that:
//
// 1. the Telegram token is defined and valid
//
// 2. the chat IDs are defined and all valid
//
// If all conditions are met, the program will log an info message about Telegram integration being enabled and will set the
// telegramEnabled field of the TomlConfig object to true.
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

// checkDiscordConf performs a minimal sanity check on the Discord configuration block, ensuring that:
//
// 1. the Discord token is defined and valid
//
// 2. the User IDs are defined and all valid
//
// If all conditions are met, the program will log an info message about Discord integration being enabled and will set the
// discordEnabled field of the TomlConfig object to true.
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

// checkUserConf does a minimal sanity check on the User configuration block, ensuring that:
//
// 1. at least one User is defined
//
// 2. all users have both username and password
//
// 3. all usernames are in proper User@domain format
//
// 4. all usernames end with @skole.hr (a warning is logged if not)
//
// If all conditions are met, the program will log an info message about User configuration being enabled.
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
