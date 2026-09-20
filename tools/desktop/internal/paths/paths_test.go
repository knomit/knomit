package paths_test

import (
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/apppaths"
	"knomit/tools/desktop/internal/paths"
)

func TestStateDir_ReturnsKnomitSubdir(t *testing.T) {
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	if !strings.HasSuffix(dir, filepath.Join("knomit")) {
		t.Errorf("StateDir = %q, want suffix 'knomit'", dir)
	}
}

func TestLockfilePath_UsesStateDir(t *testing.T) {
	state, _ := paths.StateDir()
	lock, err := paths.LockfilePath()
	if err != nil {
		t.Fatalf("LockfilePath: %v", err)
	}
	if filepath.Dir(lock) != state {
		t.Errorf("LockfilePath = %q, want in %q", lock, state)
	}
	if filepath.Base(lock) != "server.json" {
		t.Errorf("LockfilePath basename = %q, want server.json", filepath.Base(lock))
	}
}

func TestLogsDir_ReturnsKnomitSubdir(t *testing.T) {
	dir, err := paths.LogsDir()
	if err != nil {
		t.Fatalf("LogsDir: %v", err)
	}
	if !strings.HasSuffix(dir, filepath.Join("knomit")) {
		t.Errorf("LogsDir = %q, want suffix 'knomit'", dir)
	}
}

// The desktop WRITES server.json; the bridge READS it. They are separate
// binaries and the bridge cannot import this package — tools/desktop/internal/
// is importable only from under tools/desktop/ — so before internal/apppaths
// each carried its own copy of the per-OS switch, and they drifted: the
// bridge's had no Windows case at all and fell back to a default port while
// the desktop's lockfile sat unread in %LOCALAPPDATA%\knomit.
//
// Both now delegate to apppaths, and tools/bridge has the mirror of this test.
// Pinning it in both places is deliberate: either package could be "fixed"
// locally by someone who does not know the other exists, which is exactly how
// the divergence happened the first time.
func TestLockfilePath_AgreesWithAppPaths(t *testing.T) {
	want, err := apppaths.LockfilePath()
	if err != nil {
		t.Fatalf("apppaths.LockfilePath: %v", err)
	}
	got, err := paths.LockfilePath()
	if err != nil {
		t.Fatalf("paths.LockfilePath: %v", err)
	}
	if got != want {
		t.Errorf("paths.LockfilePath = %q, apppaths.LockfilePath = %q — the desktop and the bridge would look in different places", got, want)
	}
}

func TestStateDir_AgreesWithAppPaths(t *testing.T) {
	want, err := apppaths.StateDir()
	if err != nil {
		t.Fatalf("apppaths.StateDir: %v", err)
	}
	got, err := paths.StateDir()
	if err != nil {
		t.Fatalf("paths.StateDir: %v", err)
	}
	if got != want {
		t.Errorf("paths.StateDir = %q, apppaths.StateDir = %q", got, want)
	}
}
