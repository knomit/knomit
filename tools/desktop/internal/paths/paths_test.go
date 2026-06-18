package paths_test

import (
	"path/filepath"
	"strings"
	"testing"

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
