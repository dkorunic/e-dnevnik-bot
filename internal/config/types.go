// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package config

import "time"

// User is one AAI/SSO credential pair.
type User struct {
	Username string `toml:"username,omitempty"`
	Password string `toml:"password,omitempty"`
}

// Telegram messenger configuration.
type Telegram struct {
	Token   string   `toml:"token,omitempty"`
	ChatIDs []string `toml:"chatids,omitempty"`
}

// Discord messenger configuration.
type Discord struct {
	Token   string   `toml:"token,omitempty"`
	UserIDs []string `toml:"userids,omitempty"`
}

// Slack messenger configuration.
type Slack struct {
	Token   string   `toml:"token,omitempty"`
	ChatIDs []string `toml:"chatids,omitempty"`
}

// Mail messenger configuration.
type Mail struct {
	Server   string   `toml:"server,omitempty"`
	Port     string   `toml:"port,omitempty"`
	Username string   `toml:"username,omitempty"`
	Password string   `toml:"password,omitempty"`
	From     string   `toml:"from,omitempty"`
	Subject  string   `toml:"subject,omitempty"`
	To       []string `toml:"to,omitempty"`
}

// Calendar holds the Google Calendar configuration.
type Calendar struct {
	Name string `toml:"name,omitempty"`
}

// CalDAV holds a CalDAV collection and its basic-auth credentials.
type CalDAV struct {
	URL      string `toml:"url,omitempty"`
	Username string `toml:"username,omitempty"`
	Password string `toml:"password,omitempty"`
}

// WhatsApp messenger configuration.
type WhatsApp struct {
	PhoneNumber string   `toml:"phonenumber,omitempty"`
	UserIDs     []string `toml:"userids,omitempty"`
	Groups      []string `toml:"groups,omitempty"`
}

// Digest configures the weekly exam summary; on unless enabled = false.
type Digest struct {
	Hour    *int   `toml:"hour,omitempty"`    // 0–23 local; pointer distinguishes 0 from unset
	Enabled *bool  `toml:"enabled,omitempty"` // nil means on
	Day     string `toml:"day,omitempty"`     // English weekday; empty means Sunday
	// Derived by checkDigestConf.
	Weekday  time.Weekday `toml:"-"`
	SendHour int          `toml:"-"`
	Active   bool         `toml:"-"`
}

// TomlConfig is the whole configuration file.
type TomlConfig struct {
	CalDAV          CalDAV   `toml:"caldav,omitempty"`
	Calendar        Calendar `toml:"calendar,omitempty"`
	Mail            Mail     `toml:"mail,omitempty"`
	WhatsApp        WhatsApp `toml:"whatsapp,omitempty"`
	Telegram        Telegram `toml:"telegram,omitempty"`
	Discord         Discord  `toml:"discord,omitempty"`
	Slack           Slack    `toml:"slack,omitempty"`
	User            []User   `toml:"user,omitempty"`
	Digest          Digest   `toml:"digest,omitempty"`
	TelegramEnabled bool     `toml:"-"` // derived at runtime; never read from or written to TOML
	DiscordEnabled  bool     `toml:"-"`
	SlackEnabled    bool     `toml:"-"`
	MailEnabled     bool     `toml:"-"`
	CalendarEnabled bool     `toml:"-"`
	CalDAVEnabled   bool     `toml:"-"`
	WhatsAppEnabled bool     `toml:"-"`
	// Configured but not yet initialisable — a headless daemon before OAuth.
	// msgSend runs a queue-only stub so exams survive.
	CalendarDeferred bool `toml:"-"`
}
