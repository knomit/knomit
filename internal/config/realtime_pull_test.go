package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// T9 (F21 S2): [git].realtime_pull_interval defaults to 3 s, is set by the
// TOML file and overridden by KNOMIT_GIT_REALTIME_PULL_INTERVAL, and anything
// below the 1 s floor — 0, 500ms, a negative — fails Validate, so Load (boot)
// refuses it.
//
// SABOTAGE: drop the check in Validate → the 0/500ms/negative rows load → red;
// floor at 0 instead of 1 s (`< 0` or `<= 0`) → the 500ms row loads → red.
func TestConfig_RealtimePullInterval(t *testing.T) {
	if got := Defaults().Git.RealtimePullInterval; got != 3*time.Second {
		t.Fatalf("default = %s, want 3s", got)
	}
	if err := Defaults().Validate(); err != nil {
		t.Fatalf("the defaults must validate: %v", err)
	}

	load := func(t *testing.T, toml, env string) (Config, error) {
		t.Helper()
		home := t.TempDir()
		t.Setenv("KNOMIT_HOME", home)
		if toml != "" {
			if err := os.WriteFile(filepath.Join(home, "knomit.toml"), []byte(toml), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if env != "" {
			t.Setenv("KNOMIT_GIT_REALTIME_PULL_INTERVAL", env)
		}
		return Load()
	}

	t.Run("unset is the default", func(t *testing.T) {
		cfg, err := load(t, "", "")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Git.RealtimePullInterval != 3*time.Second {
			t.Fatalf("got %s", cfg.Git.RealtimePullInterval)
		}
	})
	t.Run("toml", func(t *testing.T) {
		cfg, err := load(t, "[git]\nrealtime_pull_interval = \"7s\"\n", "")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Git.RealtimePullInterval != 7*time.Second {
			t.Fatalf("got %s", cfg.Git.RealtimePullInterval)
		}
	})
	t.Run("env overrides toml", func(t *testing.T) {
		cfg, err := load(t, "[git]\nrealtime_pull_interval = \"7s\"\n", "2s")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Git.RealtimePullInterval != 2*time.Second {
			t.Fatalf("got %s", cfg.Git.RealtimePullInterval)
		}
	})
	t.Run("exactly the floor", func(t *testing.T) {
		cfg, err := load(t, "", "1s")
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Git.RealtimePullInterval != time.Second {
			t.Fatalf("got %s", cfg.Git.RealtimePullInterval)
		}
	})
	for _, bad := range []struct{ name, toml, env string }{
		{"zero", "[git]\nrealtime_pull_interval = \"0s\"\n", ""},
		{"500ms", "[git]\nrealtime_pull_interval = \"500ms\"\n", ""},
		{"negative", "", "-3s"},
		{"3ms typo", "", "3ms"},
	} {
		t.Run("refused: "+bad.name, func(t *testing.T) {
			_, err := load(t, bad.toml, bad.env)
			if err == nil {
				t.Fatal("a realtime_pull_interval below 1s must fail at boot")
			}
			if !strings.Contains(err.Error(), "realtime_pull_interval") {
				t.Fatalf("the error must name the key, got %q", err)
			}
		})
	}
}
