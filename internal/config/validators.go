// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package config

import (
	stdmail "net/mail"
	"regexp"
	"strconv"

	"go.mau.fi/whatsmeow/types"
)

var (
	phoneRegex        = regexp.MustCompile(`^\+[1-9]\d{1,14}$`)
	userAtDomainRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	// Any xox?- family token (xoxb/xoxp/xoxe/xoxa/...): the regex exists for
	// typo detection, not as a security boundary, so it accepts the whole
	// family rather than tracking Slack's per-type prefixes.
	slackTokenRegex     = regexp.MustCompile(`^xox[a-z]-(?:\d+-)+[a-zA-Z0-9]+$`)
	slackChatIDRegex    = regexp.MustCompile(`^[UWCGD][A-Z0-9]{8,}$|^\d{10}\.\d{6}$`)
	discordTokenRegex   = regexp.MustCompile(`^[MNO][a-zA-Z\d_-]{23,35}\.[a-zA-Z\d_-]{6}\.[a-zA-Z\d_-]{27,38}$`)
	telegramTokenRegex  = regexp.MustCompile(`^\d{8,10}:[0-9A-Za-z_-]{34,40}$`)
	telegramChatIDRegex = regexp.MustCompile(`^-?\d{5,19}$`)
)

// isValidPhone reports whether phone is in international format (+, 3-14 digits).
func isValidPhone(phone string) bool {
	return phoneRegex.MatchString(phone)
}

// isValidUserAtDomain reports whether user matches User@domain.tld.
func isValidUserAtDomain(user string) bool {
	return userAtDomainRegex.MatchString(user)
}

// isValidMail reports whether mail is a parseable RFC 5322 address.
func isValidMail(mail string) bool {
	_, err := stdmail.ParseAddress(mail)

	return err == nil
}

// isValidID reports whether id is an unsigned integer.
func isValidID(id string) bool {
	_, err := strconv.ParseUint(id, 10, 64)

	return err == nil
}

// isValidSlackToken reports whether token matches the Slack token format.
func isValidSlackToken(token string) bool {
	return slackTokenRegex.MatchString(token)
}

// isValidSlackChatID reports whether id matches the Slack chat ID format.
func isValidSlackChatID(id string) bool {
	return slackChatIDRegex.MatchString(id)
}

// isValidDiscordToken reports whether token matches the Discord token format.
func isValidDiscordToken(token string) bool {
	return discordTokenRegex.MatchString(token)
}

// isValidTelegramToken reports whether token matches the Telegram token format.
func isValidTelegramToken(token string) bool {
	return telegramTokenRegex.MatchString(token)
}

// isValidTelegramChatID reports whether id matches the Telegram chat ID format.
func isValidTelegramChatID(id string) bool {
	return telegramChatIDRegex.MatchString(id)
}

// isValidWhatsAppJID reports whether jid parses and targets a user or group.
// Broadcast lists are rejected: whatsmeow can't send to non-status broadcast
// lists (ErrBroadcastListUnsupported), so they only ever fail delivery.
func isValidWhatsAppJID(jid string) bool {
	parsedJID, err := types.ParseJID(jid)
	if err != nil {
		return false
	}

	return parsedJID.Server == "s.whatsapp.net" || parsedJID.Server == "g.us"
}
