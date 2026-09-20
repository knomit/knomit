package main

import (
	"path/filepath"
	"testing"

	"knomit/internal/config"
)

// The bridge finds a running server by reading the lockfile the DESKTOP wrote.
// It cannot import tools/desktop/internal/paths (Go's internal rule), so it
// used to carry its own copy of the per-OS switch — and that copy had cases
// for darwin and linux only. On Windows lockfilePath returned "unsupported
// platform windows", which readLockfileBaseURL's caller logs at Debug before
// falling back to the default base URL: `kb` talked to the wrong port, quietly,
// while the lockfile sat in %LOCALAPPDATA%\knomit.
//
// The mirror of this test lives in tools/desktop/internal/paths. Both sides
// pin the agreement because either one could be "fixed" in isolation by
// someone unaware of the other.
//
// While both sides delegate this assertion is tautological, and it is kept for
// the day one of them stops. TestLockfilePath_ResolvesOnThisPlatform below is
// the load-bearing one: it fails on a platform with no case at all, which is
// the shape the original bug had.
func TestLockfilePath_DelegatesToAppPaths(t *testing.T) {
	want, err := config.LockfilePath()
	if err != nil {
		t.Fatalf("config.LockfilePath: %v", err)
	}
	got, err := lockfilePath()
	if err != nil {
		t.Fatalf("lockfilePath: %v", err)
	}
	if got != want {
		t.Errorf("lockfilePath = %q, want %q", got, want)
	}
}

// The regression itself: on Windows this used to be an error, so the discovery
// path was dead code on the platform it most needed to work on.
func TestLockfilePath_ResolvesOnThisPlatform(t *testing.T) {
	got, err := lockfilePath()
	if err != nil {
		t.Fatalf("lockfilePath: %v — the bridge cannot discover a running server here", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("lockfilePath = %q, want an absolute path", got)
	}
	if filepath.Base(got) != "server.json" {
		t.Errorf("lockfilePath basename = %q, want server.json", filepath.Base(got))
	}
}
