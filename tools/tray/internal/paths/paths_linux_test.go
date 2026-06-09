//go:build linux

package paths_test

import (
	"os"
	"path/filepath"
	"testing"

	"knomit/tools/tray/internal/paths"
)

func TestStateDir_UsesXDGStateHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg-state-test")
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	want := filepath.Join("/tmp/xdg-state-test", "knomit")
	if dir != want {
		t.Errorf("StateDir = %q, want %q", dir, want)
	}
}

func TestStateDir_FallsBackToDefaultHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, ".local", "state", "knomit")
	if dir != want {
		t.Errorf("StateDir = %q, want %q", dir, want)
	}
}

func TestLogsDir_SameAsStateDir(t *testing.T) {
	state, err := paths.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	logs, err := paths.LogsDir()
	if err != nil {
		t.Fatalf("LogsDir: %v", err)
	}
	if state != logs {
		t.Errorf("LogsDir = %q, want same as StateDir %q", logs, state)
	}
}
