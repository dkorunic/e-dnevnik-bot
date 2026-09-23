// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package config

import (
	stdmail "net/mail"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

var (
	phoneRegex        = regexp.MustCompile(`^\+[1-9]\d{1,14}$`)
	userAtDomainRegex = regexp.MustCompile(`^[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$`)
	// The whole xox?- family. This catches typos, it is not a security
	// boundary, so it does not track Slack's per-type prefixes.
	slackTokenRegex     = regexp.MustCompile(`^xox[a-z]-(?:\d+-)+[a-zA-Z0-9]+$`)
	slackChatIDRegex    = regexp.MustCompile(`^[UWCGD][A-Z0-9]{8,}$|^\d{10}\.\d{6}$`)
	discordTokenRegex   = regexp.MustCompile(`^[MNO][a-zA-Z\d_-]{23,35}\.[a-zA-Z\d_-]{6}\.[a-zA-Z\d_-]{27,38}$`)
	telegramTokenRegex  = regexp.MustCompile(`^\d{8,10}:[0-9A-Za-z_-]{34,40}$`)
	telegramChatIDRegex = regexp.MustCompile(`^-?\d{5,19}$`)
)

// isValidPhone reports whether phone is E.164.
func isValidPhone(phone string) bool {
	return phoneRegex.MatchString(phone)
}

// isValidUserAtDomain reports whether user is User@domain.tld.
func isValidUserAtDomain(user string) bool {
	return userAtDomainRegex.MatchString(user)
}

// isValidMail reports whether mail parses as RFC 5322.
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

// isValidWhatsAppJID reports whether jid targets a user or group. Broadcast
// lists are rejected: whatsmeow cannot send to them, so they only ever fail.
func isValidWhatsAppJID(jid string) bool {
	parsedJID, err := types.ParseJID(jid)
	if err != nil {
		return false
	}

	return parsedJID.Server == "s.whatsapp.net" || parsedJID.Server == "g.us"
}

// isLoopbackHost reports whether host — as url.URL.Hostname returns it — names
// this machine: "localhost" or a loopback IP. Exact matches only, so
// localhost.evil.example is not loopback.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip, err := netip.ParseAddr(host)

	return err == nil && ip.IsLoopback()
}
