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
	Username string `toml:"username"`
	Password string `toml:"password"`
}

// Telegram struct holds Telegram messenger configuration.
type Telegram struct {
	Token   string   `toml:"token"`
	ChatIDs []string `toml:"chatids"`
}

// Discord struct holds Discord messenger configuration.
type Discord struct {
	Token   string   `toml:"token"`
	UserIDs []string `toml:"userids"`
}

// Slack struct holds Slack messenger configuration.
type Slack struct {
	Token   string   `toml:"token"`
	ChatIDs []string `toml:"chatids"`
}

// Mail struct hold e-Mail messenger configuration.
type Mail struct {
	Server   string   `toml:"server"`
	Port     string   `toml:"port"`
	Username string   `toml:"username"`
	Password string   `toml:"password"`
	From     string   `toml:"from"`
	Subject  string   `toml:"subject"`
	To       []string `toml:"to"`
}

// Calendar struct hold Google Calendar configuration.
type Calendar struct {
	Name string `toml:"name"`
}

// WhatsApp struct holds WhatsApp messenger configuration.
type WhatsApp struct {
	PhoneNumber string   `toml:"phonenumber"`
	UserIDs     []string `toml:"userids"`
	Groups      []string `toml:"groups"`
}

// TomlConfig struct holds all other configuration structures.
type TomlConfig struct {
	Calendar        Calendar `toml:"calendar"`
	Mail            Mail     `toml:"mail"`
	Telegram        Telegram `toml:"telegram"`
	Discord         Discord  `toml:"discord"`
	Slack           Slack    `toml:"slack"`
	User            []User   `toml:"user"`
	WhatsApp        WhatsApp `toml:"whatsapp"`
	TelegramEnabled bool     `toml:"telegram_enabled"`
	DiscordEnabled  bool     `toml:"discord_enabled"`
	SlackEnabled    bool     `toml:"slack_enabled"`
	MailEnabled     bool     `toml:"mail_enabled"`
	CalendarEnabled bool     `toml:"calendar_enabled"`
	WhatsAppEnabled bool     `toml:"whatsapp_enabled"`
}
