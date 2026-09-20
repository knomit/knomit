//go:build windows

package config_test

import (
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/apppaths"
	"knomit/internal/config"
)

// scrubHome removes every variable that could resolve a data root, so Load has
// nothing to fall back to. KNOMIT_HOME and KNOMIT_REPO are cleared too: the
// developer running this suite almost certainly has one set.
func scrubHome(t *testing.T) {
	t.Helper()
	for _, k := range []string{"KNOMIT_HOME", "KNOMIT_REPO", "LOCALAPPDATA", "USERPROFILE", "HOME"} {
		t.Setenv(k, "")
	}
}

// THE REGRESSION TEST. Before this change Defaults() did
//
//	home, _ := os.UserHomeDir()
//	Home: home + "/.knomit"
//
// so a process whose environment lacked %USERPROFILE% got Home = "/.knomit",
// which Windows resolves against the current drive: C:\.knomit. It is writable,
// so nothing failed — the server generated a fresh SSH identity there and
// started re-downloading 617MB of models into a directory the user would never
// think to look in.
//
// Verified to FAIL against the pre-change config.go (it returned a nil error
// and a rooted path) before being kept.
func TestLoad_FailsWhenNoHomeCanBeResolved(t *testing.T) {
	scrubHome(t)

	cfg, err := config.Load()
	if err == nil {
		t.Fatalf("Load() succeeded with Home = %q; want an error when no home resolves", cfg.Home)
	}
	// An actionable message, not just a failure: the operator has to be told
	// the one thing that fixes it.
	if !strings.Contains(err.Error(), "KNOMIT_HOME") {
		t.Errorf("error %q does not name KNOMIT_HOME", err)
	}
	// And nothing usable must come back beside the error, or a caller that
	// logs-and-continues reintroduces the bug.
	if cfg.Home != "" {
		t.Errorf("Load() returned Home = %q with its error; want \"\"", cfg.Home)
	}
}

// The specific shape of the old bug, pinned independently of the error
// contract: whatever Load does when it cannot resolve a home, it must never
// hand back a path anchored at the root of the current drive.
func TestLoad_NeverYieldsADriveRootHome(t *testing.T) {
	scrubHome(t)

	cfg, err := config.Load()
	if err != nil {
		return // the documented outcome; the assertion below is the backstop
	}
	if cfg.Home == `\.knomit` || cfg.Home == "/.knomit" {
		t.Fatalf("Load() produced the drive-root home %q — the C:\\.knomit regression", cfg.Home)
	}
	if !filepath.IsAbs(cfg.Home) {
		t.Errorf("Load() produced a non-absolute Home %q", cfg.Home)
	}
}

// The Windows default is whatever apppaths says, with no second spelling in
// config. If these two ever disagree, `knomit serve` and the desktop resolve
// different data roots from the same machine.
func TestLoad_WindowsDefaultComesFromAppPaths(t *testing.T) {
	for _, k := range []string{"KNOMIT_HOME", "KNOMIT_REPO"} {
		t.Setenv(k, "")
	}
	t.Setenv("LOCALAPPDATA", t.TempDir())

	want, err := apppaths.DefaultHome()
	if err != nil {
		t.Fatalf("apppaths.DefaultHome: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Home != want {
		t.Errorf("Load().Home = %q, want apppaths.DefaultHome() = %q", cfg.Home, want)
	}
	if filepath.Base(cfg.Home) != "home" {
		t.Errorf("Load().Home = %q, want it to end in the 'home' subdir", cfg.Home)
	}
}

// KNOMIT_HOME still wins over the new default — the single-root convention is
// unchanged by this move.
func TestLoad_KnomitHomeStillOverridesTheWindowsDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("LOCALAPPDATA", t.TempDir())
	t.Setenv("KNOMIT_REPO", "")
	t.Setenv("KNOMIT_HOME", dir)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Home != dir {
		t.Errorf("Load().Home = %q, want the KNOMIT_HOME value %q", cfg.Home, dir)
	}
}
