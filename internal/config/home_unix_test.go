//go:build !windows

package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
)

// Same contract as the Windows file, with the variables this platform actually
// consults. os.UserHomeDir reads $HOME here and %USERPROFILE% there, which is
// the whole reason the two tests are separate rather than one table: a row
// that scrubbed the wrong variable would pass while proving nothing.
func TestLoad_FailsWhenNoHomeCanBeResolved(t *testing.T) {
	for _, k := range []string{"KNOMIT_HOME", "KNOMIT_REPO", "HOME"} {
		t.Setenv(k, "")
	}

	cfg, err := config.Load()
	if err == nil {
		if cfg.Home == "/.knomit" {
			t.Fatalf("Load() produced the root-relative home %q", cfg.Home)
		}
		t.Skipf("this platform resolved a home (%q) without $HOME", cfg.Home)
	}
	if !strings.Contains(err.Error(), "KNOMIT_HOME") {
		t.Errorf("error %q does not name KNOMIT_HOME", err)
	}
	if cfg.Home != "" {
		t.Errorf("Load() returned Home = %q with its error; want \"\"", cfg.Home)
	}
}

// ~/.knomit on this platform, unchanged, and still sourced from the one helper.
func TestLoad_DefaultIsDotKnomitFromAppPaths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("KNOMIT_HOME", "")
	t.Setenv("KNOMIT_REPO", "")
	t.Setenv("HOME", dir)

	want, err := config.DefaultHome()
	if err != nil {
		t.Fatalf("config.DefaultHome: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Home != want {
		t.Errorf("Load().Home = %q, want config.DefaultHome() = %q", cfg.Home, want)
	}
	if cfg.Home != filepath.Join(dir, ".knomit") {
		t.Errorf("Load().Home = %q, want %q — the unix default must not have moved",
			cfg.Home, filepath.Join(dir, ".knomit"))
	}
}
