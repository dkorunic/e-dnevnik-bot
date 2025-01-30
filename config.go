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

package main

import (
	stdmail "net/mail"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/dkorunic/e-dnevnik-bot/logger"
	"go.mau.fi/whatsmeow/types"
)

// user struct holds a single AAI/SSO username.
type user struct {
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// telegram struct holds Telegram messenger configuration.
type telegram struct {
	Token   string   `toml:"token"`
	ChatIDs []string `toml:"chatids"`
}

// discord struct holds Discord messenger configuration.
type discord struct {
	Token   string   `toml:"token"`
	UserIDs []string `toml:"userids"`
}

// slack struct holds Slack messenger configuration.
type slack struct {
	Token   string   `toml:"token"`
	ChatIDs []string `toml:"chatids"`
}

// mail struct hold e-mail messenger configuration.
type mail struct {
	Server   string   `toml:"server"`
	Port     string   `toml:"port"`
	Username string   `toml:"username"`
	Password string   `toml:"password"`
	From     string   `toml:"from"`
	Subject  string   `toml:"subject"`
	To       []string `toml:"to"`
}

// calendar struct hold Google Calendar configuration.
type calendar struct {
	Name string `toml:"name"`
}

// whatsapp struct holds WhatsApp messenger configuration.
type whatsapp struct {
	PhoneNumber string   `toml:"phonenumber"`
	UserIDs     []string `toml:"userids"`
	Groups      []string `toml:"groups"`
}

// tomlConfig struct holds all other configuration structures.
type tomlConfig struct {
	Calendar        calendar `toml:"calendar"`
	Mail            mail     `toml:"mail"`
	Telegram        telegram `toml:"telegram"`
	Discord         discord  `toml:"discord"`
	Slack           slack    `toml:"slack"`
	User            []user   `toml:"user"`
	WhatsApp        whatsapp `toml:"whatsapp"`
	telegramEnabled bool     `toml:"telegram_enabled"`
	discordEnabled  bool     `toml:"discord_enabled"`
	slackEnabled    bool     `toml:"slack_enabled"`
	mailEnabled     bool     `toml:"mail_enabled"`
	calendarEnabled bool     `toml:"calendar_enabled"`
	whatsAppEnabled bool     `toml:"whatsapp_enabled"`
}

var (
	phoneRegex          = regexp.MustCompile(`^\+[1-9][0-9]{3,14}$`)
	slackTokenRegex     = regexp.MustCompile(`^xox(?i:[abposr])-(?:\d+-)+[a-z0-9]+$`)
	slackChatIDRegex    = regexp.MustCompile(`^[UWCGD][A-Z0-9]{8,}$|^\d{10}\.\d{6}$`)
	discordTokenRegex   = regexp.MustCompile(`^[MNO][a-zA-Z\d_-]{23,25}\.[a-zA-Z\d_-]{6}\.[a-zA-Z\d_-]{27}$`)
	telegramTokenRegex  = regexp.MustCompile(`^\d{8,10}:[0-9A-Za-z_-]{35}$`)
	telegramChatIDRegex = regexp.MustCompile(`^-?\d{5,15}$`)
)

// loadConfig attempts to load and decode configuration file in TOML format, doing a minimal sanity checking and
// optionally returning an error.
func loadConfig() (tomlConfig, error) {
	var config tomlConfig
	if _, err := toml.DecodeFile(*confFile, &config); err != nil {
		return config, err
	}

	// check if users are defined
	if len(config.User) == 0 {
		logger.Fatal().Msg("Configuration error: No users defined")
	}

	// check if all users have username and password
	for _, u := range config.User {
		if u.Username == "" || u.Password == "" {
			logger.Fatal().Msgf("Configuration error: user requires username and password: %q - %q",
				u.Username, u.Password)
		}

		// check if username is in e-mail format
		if !isValidMail(u.Username) {
			logger.Fatal().Msgf("Configuration error: username not in e-mail format: %q", u.Username)
		}

		// check if username ends with @skole.hr
		if !strings.HasSuffix(u.Username, "@skole.hr") {
			logger.Warn().Msgf("Configuration issue: username not ending with @skole.hr: %q", u.Username)
		}
	}

	// check if Discord token is defined
	if config.Discord.Token != "" {
		// check if token is valid
		if !isValidDiscordToken(config.Discord.Token) {
			logger.Fatal().Msgf("Configuration error: Discord token %q is not valid", config.Discord.Token)
		}

		// check if user IDs are defined
		if len(config.Discord.UserIDs) == 0 {
			logger.Fatal().Msg("Configuration error: Discord user IDs not defined")
		}

		// check if all user IDs are valid
		for _, u := range config.Discord.UserIDs {
			if !isValidID(u) {
				logger.Fatal().Msgf("Configuration error: Discord user ID %q is not valid", u)
			}
		}

		logger.Info().Msg("Configuration: Discord messenger enabled")

		config.discordEnabled = true
	}

	// check if Telegram token is defined
	if config.Telegram.Token != "" {
		// check if token is valid
		if !isValidTelegramToken(config.Telegram.Token) {
			logger.Fatal().Msgf("Configuration error: Telegram token %q is not valid", config.Telegram.Token)
		}

		// check if user IDs are defined
		if len(config.Telegram.ChatIDs) == 0 {
			logger.Fatal().Msg("Configuration error: Telegram chat IDs not defined")
		}

		// check if all chat IDs are valid
		for _, c := range config.Telegram.ChatIDs {
			if !isValidTelegramChatID(c) {
				logger.Fatal().Msgf("Configuration error: Telegram chat ID %q is not valid", c)
			}
		}

		logger.Info().Msg("Configuration: Telegram messenger enabled")

		config.telegramEnabled = true
	}

	// check if Slack token is defined
	if config.Slack.Token != "" {
		// check if token is valid
		if !isValidSlackToken(config.Slack.Token) {
			logger.Fatal().Msgf("Configuration error: Slack token %q is not valid", config.Slack.Token)
		}

		// check if user IDs are defined
		if len(config.Slack.ChatIDs) == 0 {
			logger.Fatal().Msg("Configuration error: Slack chat IDs not defined")
		}

		// check if all chat IDs are valid
		for _, c := range config.Slack.ChatIDs {
			if !isValidSlackChatID(c) {
				logger.Fatal().Msgf("Configuration error: Slack chat ID %q is not valid", c)
			}
		}

		logger.Info().Msg("Configuration: Slack messenger enabled")

		config.slackEnabled = true
	}

	if config.Mail.Server != "" {
		// check if any of e-mail destinations (TO) are defined
		if len(config.Mail.To) == 0 {
			logger.Fatal().Msg("Configuration error: no e-mail to addresses defined")
		}

		logger.Info().Msg("Configuration: e-mail messenger enabled (pending check during initialization)")

		// no need to check e-mail FROM since it can be anything

		// check if all destination e-mail addresses are valid
		for _, t := range config.Mail.To {
			if !isValidMail(t) {
				logger.Fatal().Msgf("Configuration error: e-mail to %q is not in e-mail format", t)
			}
		}

		config.mailEnabled = true
	}

	if config.Calendar.Name != "" {
		config.calendarEnabled = true
	}

	if len(config.WhatsApp.UserIDs) > 0 || len(config.WhatsApp.Groups) > 0 {
		// check if phone number is valid
		if config.WhatsApp.PhoneNumber != "" && !isValidPhone(config.WhatsApp.PhoneNumber) {
			logger.Fatal().Msgf("WhatsApp phone number %s is not in international format",
				config.WhatsApp.PhoneNumber)
		}

		// check if all user IDs are valid
		for _, u := range config.WhatsApp.UserIDs {
			if !isValidWhatsAppJID(u) {
				logger.Fatal().Msgf("Configuration error: WhatsApp user ID %q is not valid", u)
			}
		}

		logger.Info().Msg("Configuration: Whatsapp messenger enabled (pending check during initialization)")

		// sort group names for binary search in WhatsApp messenger
		slices.SortStableFunc(config.WhatsApp.Groups, strings.Compare)

		config.whatsAppEnabled = true
	}

	return config, nil
}

// isValidPhone checks if the given phone number is in international format
// (starting with "+", followed by 3-14 digits).
//
// Parameters:
// - phone: the phone number to validate
//
// Returns:
// - true if phone number is valid, false otherwise
func isValidPhone(phone string) bool {
	return phoneRegex.MatchString(phone)
}

// isValidMail checks if the given string is a valid e-mail address in the
// format user@domain.tld.
//
// Parameters:
// - mail: the e-mail address to validate
//
// Returns:
// - true if the e-mail address is valid, false otherwise
func isValidMail(mail string) bool {
	_, err := stdmail.ParseAddress(mail)

	return err == nil
}

// isValidID checks if the given string is a valid positive integer.
//
// Parameters:
// - id: the string to validate
//
// Returns:
// - true if the string is a valid positive integer, false otherwise
func isValidID(id string) bool {
	_, err := strconv.ParseUint(id, 10, 64)

	return err == nil
}

// isValidSlackToken checks if the given string is a valid Slack token.
//
// Parameters:
// - token: the Slack token to validate
//
// Returns:
// - true if the Slack token is valid, false otherwise
func isValidSlackToken(token string) bool {
	return slackTokenRegex.MatchString(token)
}

// isValidSlackChatID checks if the given string is a valid Slack Chat ID.
//
// Parameters:
// - id: the string to validate
//
// Returns:
// - true if the string is a valid Slack Chat ID, false otherwise
func isValidSlackChatID(id string) bool {
	return slackChatIDRegex.MatchString(id)
}

// isValidDiscordToken checks if the given string is a valid Discord token.
//
// Parameters:
// - token: the Discord token to validate
//
// Returns:
// - true if the Discord token is valid, false otherwise
func isValidDiscordToken(token string) bool {
	return discordTokenRegex.MatchString(token)
}

// isValidTelegramToken checks if the given string is a valid Telegram token.
//
// Parameters:
// - token: the Telegram token to validate
//
// Returns:
// - true if the Telegram token is valid, false otherwise
func isValidTelegramToken(token string) bool {
	return telegramTokenRegex.MatchString(token)
}

// isValidTelegramChatID checks if the given string is a valid Telegram Chat ID.
//
// Parameters:
// - id: the string to validate
//
// Returns:
// - true if the string is a valid Telegram Chat ID, false otherwise
func isValidTelegramChatID(id string) bool {
	return telegramChatIDRegex.MatchString(id)
}

// isValidWhatsAppJID checks if the given string is a valid WhatsApp group or user JID.
//
// Parameters:
// - jid: the string to validate
//
// Returns:
// - true if the string is a valid WhatsApp JID, false otherwise
func isValidWhatsAppJID(jid string) bool {
	_, err := types.ParseJID(jid)

	return err == nil
}
