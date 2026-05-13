// SPDX-FileCopyrightText: 2025 Dinko Korunic
// SPDX-License-Identifier: MIT

package config

// User struct holds a single AAI/SSO username.
type User struct {
	Username string `toml:"username,omitempty"`
	Password string `toml:"password,omitempty"`
}

// Telegram struct holds Telegram messenger configuration.
type Telegram struct {
	Token   string   `toml:"token,omitempty"`
	ChatIDs []string `toml:"chatids,omitempty"`
}

// Discord struct holds Discord messenger configuration.
type Discord struct {
	Token   string   `toml:"token,omitempty"`
	UserIDs []string `toml:"userids,omitempty"`
}

// Slack struct holds Slack messenger configuration.
type Slack struct {
	Token   string   `toml:"token,omitempty"`
	ChatIDs []string `toml:"chatids,omitempty"`
}

// Mail struct hold mail messenger configuration.
type Mail struct {
	Server   string   `toml:"server,omitempty"`
	Port     string   `toml:"port,omitempty"`
	Username string   `toml:"username,omitempty"`
	Password string   `toml:"password,omitempty"`
	From     string   `toml:"from,omitempty"`
	Subject  string   `toml:"subject,omitempty"`
	To       []string `toml:"to,omitempty"`
}

// Calendar struct hold Google Calendar configuration.
type Calendar struct {
	Name string `toml:"name,omitempty"`
}

// WhatsApp struct holds WhatsApp messenger configuration.
type WhatsApp struct {
	PhoneNumber string   `toml:"phonenumber,omitempty"`
	UserIDs     []string `toml:"userids,omitempty"`
	Groups      []string `toml:"groups,omitempty"`
}

// TomlConfig struct holds all other configuration structures.
type TomlConfig struct {
	Calendar        Calendar `toml:"calendar,omitempty"`
	Mail            Mail     `toml:"mail,omitempty"`
	Telegram        Telegram `toml:"telegram,omitempty"`
	Discord         Discord  `toml:"discord,omitempty"`
	Slack           Slack    `toml:"slack,omitempty"`
	User            []User   `toml:"user,omitempty"`
	WhatsApp        WhatsApp `toml:"whatsapp,omitempty"`
	TelegramEnabled bool     `toml:"-"` // derived at runtime; never read from or written to TOML
	DiscordEnabled  bool     `toml:"-"`
	SlackEnabled    bool     `toml:"-"`
	MailEnabled     bool     `toml:"-"`
	CalendarEnabled bool     `toml:"-"`
	WhatsAppEnabled bool     `toml:"-"`
}
