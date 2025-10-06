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
	TelegramEnabled bool     `toml:"telegram_enabled,omitempty"`
	DiscordEnabled  bool     `toml:"discord_enabled,omitempty"`
	SlackEnabled    bool     `toml:"slack_enabled,omitempty"`
	MailEnabled     bool     `toml:"mail_enabled,omitempty"`
	CalendarEnabled bool     `toml:"calendar_enabled,omitempty"`
	WhatsAppEnabled bool     `toml:"whatsapp_enabled,omitempty"`
}
