// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package config

import (
	"strings"
	"time"

	"github.com/dkorunic/e-dnevnik-bot/internal/logger"
)

const (
	DefaultDigestDay  = time.Sunday
	DefaultDigestHour = 18 // catches exams entered over the weekend
)

// checkDigestConf resolves digest settings; fatal on an invalid day or hour.
// Disabled blocks skip validation.
func checkDigestConf(config *TomlConfig) {
	if config.Digest.Enabled != nil && !*config.Digest.Enabled {
		logger.Info().Msg("Configuration: weekly exam digest disabled")

		return
	}

	config.Digest.Active = true

	config.Digest.Weekday = DefaultDigestDay

	if day := strings.TrimSpace(config.Digest.Day); day != "" {
		wd, ok := parseWeekday(day)
		if !ok {
			logger.Fatal().Msgf("Configuration error: digest day %q is not an English weekday name", config.Digest.Day)
		}

		config.Digest.Weekday = wd
	}

	config.Digest.SendHour = DefaultDigestHour

	if config.Digest.Hour != nil {
		if *config.Digest.Hour < 0 || *config.Digest.Hour > 23 {
			logger.Fatal().Msgf("Configuration error: digest hour %d is outside 0-23", *config.Digest.Hour)
		}

		config.Digest.SendHour = *config.Digest.Hour
	}

	logger.Info().Msgf("Configuration: weekly exam digest enabled, sent %v from %02d:00",
		config.Digest.Weekday, config.Digest.SendHour)
}

// parseWeekday matches an English weekday name case-insensitively.
func parseWeekday(s string) (time.Weekday, bool) {
	for wd := time.Sunday; wd <= time.Saturday; wd++ {
		if strings.EqualFold(s, wd.String()) {
			return wd, true
		}
	}

	return 0, false
}
