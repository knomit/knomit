package config

import (
	"strings"
	"testing"
)

// ScriptRatePerMinute (F07 PR 3, ruling D-rate): the per-trigger script cap
// defaults to 60 when the key is absent, takes the TOML value when set, the env
// var over that, and refuses 0 or a negative at Load — "0" is neither
// "unlimited" nor "nothing runs". Sabotage: drop the key from Defaults (absent
// → 0 → Load fails); ignore the TOML value (set → 60).
func TestLoad_ScriptRatePerMinute_DefaultTOMLEnvInvalid(t *testing.T) {
	writeConfig(t, "[log]\nlevel = \"info\"\n")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Triggers.ScriptRatePerMinute != 60 {
		t.Fatalf("absent key: want the built-in 60, got %d", cfg.Triggers.ScriptRatePerMinute)
	}

	writeConfig(t, "[triggers]\nscript_rate_per_minute = 5\n")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Triggers.ScriptRatePerMinute != 5 {
		t.Fatalf("TOML script_rate_per_minute: want 5, got %d", cfg.Triggers.ScriptRatePerMinute)
	}

	t.Setenv("KNOMIT_TRIGGERS_SCRIPT_RATE_PER_MINUTE", "7")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load with env: %v", err)
	}
	if cfg.Triggers.ScriptRatePerMinute != 7 {
		t.Fatalf("env must override the TOML value, got %d", cfg.Triggers.ScriptRatePerMinute)
	}

	for _, bad := range []string{"0", "-3"} {
		t.Setenv("KNOMIT_TRIGGERS_SCRIPT_RATE_PER_MINUTE", bad)
		_, err = Load()
		if err == nil || !strings.Contains(err.Error(), "triggers.script_rate_per_minute must be >= 1") {
			t.Fatalf("value %s must fail Load naming the key, got %v", bad, err)
		}
	}
}
