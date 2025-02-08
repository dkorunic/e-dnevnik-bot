// @license
// Copyright (C) 2025  Dinko Korunic
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

//nolint:godot
package config

import (
	stdmail "net/mail"
	"regexp"
	"strconv"

	"go.mau.fi/whatsmeow/types"
)

var (
	phoneRegex          = regexp.MustCompile(`^\+[1-9][0-9]{3,14}$`)
	userAtDomainRegex   = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	slackTokenRegex     = regexp.MustCompile(`^xox(?i:[abposr])-(?:\d+-)+[a-zA-Z0-9]+$`)
	slackChatIDRegex    = regexp.MustCompile(`^[UWCGD][A-Z0-9]{8,}$|^\d{10}\.\d{6}$`)
	discordTokenRegex   = regexp.MustCompile(`^[MNO][a-zA-Z\d_-]{23,25}\.[a-zA-Z\d_-]{6}\.[a-zA-Z\d_-]{27}$`)
	telegramTokenRegex  = regexp.MustCompile(`^\d{8,10}:[0-9A-Za-z_-]{35}$`)
	telegramChatIDRegex = regexp.MustCompile(`^-?\d{5,15}$`)
)

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

// isValidUserAtDomain checks if the given string is a valid username at domain
// (User@domain.tld).
//
// Parameters:
// - User: the username at domain to validate
//
// Returns:
// - true if the username at domain is valid, false otherwise
func isValidUserAtDomain(user string) bool {
	return userAtDomainRegex.MatchString(user)
}

// isValidMail checks if the given string is a valid mail address in the
// format User@domain.tld.
//
// Parameters:
// - Mail: the mail address to validate
//
// Returns:
// - true if the mail address is valid, false otherwise
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

// isValidWhatsAppJID checks if the given string is a valid WhatsApp group or User JID.
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
