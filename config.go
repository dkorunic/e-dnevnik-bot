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
	"github.com/BurntSushi/toml"
	"github.com/dkorunic/e-dnevnik-bot/logger"
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

// tomlConfig struct holds all other configuration structures.
type tomlConfig struct {
	User            []user   `toml:"user"`
	Telegram        telegram `toml:"telegram"`
	Discord         discord  `toml:"discord"`
	Slack           slack    `toml:"slack"`
	Mail            mail     `toml:"mail"`
	telegramEnabled bool     `toml:"telegram_enabled"`
	discordEnabled  bool     `toml:"discord_enabled"`
	slackEnabled    bool     `toml:"slack_enabled"`
	mailEnabled     bool     `toml:"mail_enabled"`
}

// loadConfig attempts to load and decode configuration file in TOML format, doing a minimal sanity checking and
// optionally returning an error.
func loadConfig() (tomlConfig, error) {
	var config tomlConfig
	if _, err := toml.DecodeFile(*confFile, &config); err != nil {
		return config, err
	}

	if config.Discord.Token != "" && len(config.Discord.UserIDs) > 0 {
		logger.Info().Msg("Configuration: Discord messenger enabled")

		config.discordEnabled = true
	}

	if config.Telegram.Token != "" && len(config.Telegram.ChatIDs) > 0 {
		logger.Info().Msg("Configuration: Telegram messenger enabled")

		config.telegramEnabled = true
	}

	if config.Slack.Token != "" && len(config.Slack.ChatIDs) > 0 {
		logger.Info().Msg("Configuration: Slack messenger enabled")

		config.slackEnabled = true
	}

	if config.Mail.Server != "" && config.Mail.From != "" && len(config.Mail.To) > 0 {
		logger.Info().Msg("Configuration: e-mail messenger enabled")

		config.mailEnabled = true
	}

	return config, nil
}
