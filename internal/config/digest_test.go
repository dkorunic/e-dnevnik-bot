// SPDX-FileCopyrightText: 2026 Dinko Korunic
// SPDX-License-Identifier: MIT

package config

import (
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

// TestCheckDigestConf pins defaults and that an explicit hour 0 survives.
func TestCheckDigestConf(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		in       Digest
		wantDay  time.Weekday
		wantHour int
	}{
		{name: "absent block", in: Digest{}, wantDay: time.Sunday, wantHour: DefaultDigestHour},
		{name: "explicitly enabled", in: Digest{Enabled: new(true)}, wantDay: time.Sunday, wantHour: DefaultDigestHour},
		{name: "explicit", in: Digest{Day: "Friday", Hour: new(7)}, wantDay: time.Friday, wantHour: 7},
		{name: "midnight", in: Digest{Day: " MONDAY ", Hour: new(0)}, wantDay: time.Monday, wantHour: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := TomlConfig{Digest: tt.in}
			checkDigestConf(&cfg)

			if !cfg.Digest.Active {
				t.Errorf("checkDigestConf(%+v) left the digest inactive; it is on by default", tt.in)
			}

			if cfg.Digest.Weekday != tt.wantDay || cfg.Digest.SendHour != tt.wantHour {
				t.Errorf("checkDigestConf(%+v) = %v@%d, want %v@%d",
					tt.in, cfg.Digest.Weekday, cfg.Digest.SendHour, tt.wantDay, tt.wantHour)
			}
		})
	}
}

// TestCheckDigestConfDisabledSkipsValidation: disabled blocks must not abort startup.
func TestCheckDigestConfDisabledSkipsValidation(t *testing.T) {
	t.Parallel()

	cfg := TomlConfig{Digest: Digest{Enabled: new(false), Day: "bogus", Hour: new(99)}}
	checkDigestConf(&cfg)

	if cfg.Digest.Active {
		t.Error("a disabled digest block came back enabled")
	}
}

// TestDigestTOMLDecoding: absent means on, false means off, via the real decoder.
func TestDigestTOMLDecoding(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		toml string
		want bool
	}{
		{name: "no block", toml: "", want: true},
		{name: "block without enabled", toml: "[digest]\nday = \"friday\"\n", want: true},
		{name: "enabled true", toml: "[digest]\nenabled = true\n", want: true},
		{name: "enabled false", toml: "[digest]\nenabled = false\n", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var cfg TomlConfig
			if _, err := toml.Decode(tt.toml, &cfg); err != nil {
				t.Fatalf("toml.Decode() failed: %v", err)
			}

			checkDigestConf(&cfg)

			if cfg.Digest.Active != tt.want {
				t.Errorf("Active = %v, want %v", cfg.Digest.Active, tt.want)
			}
		})
	}
}
