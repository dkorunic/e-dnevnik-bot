// @license
// Copyright (C) 2022  Dinko Korunic
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
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package config

import (
	"bytes"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"github.com/google/renameio/v2/maybe"
)

// LoadConfig attempts to load and decode configuration file in TOML format, doing a minimal sanity checking and
// optionally returning an error.
func LoadConfig(file string) (TomlConfig, error) {
	var config TomlConfig
	if _, err := toml.DecodeFile(file, &config); err != nil {
		return config, err
	}

	checkUserConf(&config)
	checkDiscordConf(&config)
	checkTelegramConf(&config)
	checkSlackConf(&config)
	checkMailConf(&config)
	checkCalendarConf(&config)
	checkWhatsAppConf(&config)

	if !config.WhatsAppEnabled && !config.DiscordEnabled && !config.TelegramEnabled && !config.SlackEnabled &&
		!config.MailEnabled && !config.CalendarEnabled {
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

	if err := maybe.WriteFile(file, buf.Bytes(), 0o644); err != nil {
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
		// checkWhatsAppConf if phone number is valid
		if config.WhatsApp.PhoneNumber != "" && !isValidPhone(config.WhatsApp.PhoneNumber) {
			logger.Fatal().Msgf("WhatsApp phone number %s is not in international format",
				config.WhatsApp.PhoneNumber)
		}

		// checkWhatsAppConf if all User IDs are valid
		for _, u := range config.WhatsApp.UserIDs {
			if !isValidWhatsAppJID(u) {
				logger.Fatal().Msgf("Configuration error: WhatsApp User ID %v is not valid", u)
			}
		}

		logger.Info().Msg("Configuration: Whatsapp messenger enabled (pending checkWhatsAppConf during initialization)")

		// sort group names for binary search in WhatsApp messenger
		slices.SortStableFunc(config.WhatsApp.Groups, strings.Compare)

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
		// checkWhatsAppConf if any of mail destinations (TO) are defined
		if len(config.Mail.To) == 0 {
			logger.Fatal().Msg("Configuration error: no mail to addresses defined")
		}

		logger.Info().Msg("Configuration: mail messenger enabled (pending checkWhatsAppConf during initialization)")

		// no need to checkWhatsAppConf mail FROM since it can be anything

		// checkWhatsAppConf if all destination mail addresses are valid
		for _, t := range config.Mail.To {
			if !isValidMail(t) {
				logger.Fatal().Msgf("Configuration error: mail to %v is not in mail format", t)
			}
		}

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
	// checkWhatsAppConf if Slack token is defined
	if config.Slack.Token != "" {
		// checkWhatsAppConf if token is valid
		if !isValidSlackToken(config.Slack.Token) {
			logger.Fatal().Msgf("Configuration error: Slack token %v is not valid", config.Slack.Token)
		}

		// checkWhatsAppConf if User IDs are defined
		if len(config.Slack.ChatIDs) == 0 {
			logger.Fatal().Msg("Configuration error: Slack chat IDs not defined")
		}

		// checkWhatsAppConf if all chat IDs are valid
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
	// checkWhatsAppConf if Telegram token is defined
	if config.Telegram.Token != "" {
		// checkWhatsAppConf if token is valid
		if !isValidTelegramToken(config.Telegram.Token) {
			logger.Fatal().Msgf("Configuration error: Telegram token %v is not valid", config.Telegram.Token)
		}

		// checkWhatsAppConf if User IDs are defined
		if len(config.Telegram.ChatIDs) == 0 {
			logger.Fatal().Msg("Configuration error: Telegram chat IDs not defined")
		}

		// checkWhatsAppConf if all chat IDs are valid
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
	// checkWhatsAppConf if Discord token is defined
	if config.Discord.Token != "" {
		// checkWhatsAppConf if token is valid
		if !isValidDiscordToken(config.Discord.Token) {
			logger.Fatal().Msgf("Configuration error: Discord token %v is not valid", config.Discord.Token)
		}

		// checkWhatsAppConf if User IDs are defined
		if len(config.Discord.UserIDs) == 0 {
			logger.Fatal().Msg("Configuration error: Discord User IDs not defined")
		}

		// checkWhatsAppConf if all User IDs are valid
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
	// checkWhatsAppConf if users are defined
	if len(config.User) == 0 {
		logger.Fatal().Msg("Configuration error: No users defined")
	}

	// checkWhatsAppConf if all users have username and password
	for _, u := range config.User {
		if u.Username == "" || u.Password == "" {
			logger.Fatal().Msgf("Configuration error: User requires username and password: %v - %v",
				u.Username, u.Password)
		}

		// checkWhatsAppConf if username is in User@domain format
		if !isValidUserAtDomain(u.Username) {
			logger.Fatal().Msgf("Configuration error: username not in proper User@domain format: %v", u.Username)
		}

		// checkWhatsAppConf if username ends with @skole.hr
		if !strings.HasSuffix(u.Username, "@skole.hr") {
			logger.Warn().Msgf("Configuration issue: username not ending with @skole.hr: %v", u.Username)
		}
	}
}
