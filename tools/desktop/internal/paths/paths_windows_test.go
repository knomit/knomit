//go:build windows

package paths_test

import (
	"os"
	"path/filepath"
	"testing"

	"knomit/tools/desktop/internal/paths"
)

func TestStateDir_UsesLocalAppData(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `C:\Users\test\AppData\Local`)
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	want := filepath.Join(`C:\Users\test\AppData\Local`, "knomit")
	if dir != want {
		t.Errorf("StateDir = %q, want %q", dir, want)
	}
}

// The roaming profile is the wrong home for server.json and update.json: they
// describe the machine, not the user, and roaming would copy one machine's
// port, PID and half-finished update onto another. This pins that the local
// directory is what is used when the two differ.
func TestStateDir_PrefersLocalOverRoaming(t *testing.T) {
	t.Setenv("LOCALAPPDATA", `C:\Users\test\AppData\Local`)
	t.Setenv("APPDATA", `C:\Users\test\AppData\Roaming`)
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	if filepath.Dir(dir) != `C:\Users\test\AppData\Local` {
		t.Errorf("StateDir = %q, want it under the Local profile", dir)
	}
}

// LOCALAPPDATA is set in any normal interactive session but can be absent
// under a service account. Falling back to the documented default beats
// returning an error the desktop app cannot act on.
func TestStateDir_FallsBackWhenLocalAppDataIsUnset(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no user home on this machine: %v", err)
	}
	want := filepath.Join(home, "AppData", "Local", "knomit")
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

// The regression this package existed to cause: every one of these returned
// "unsupported platform windows (phase 1 is macOS only)", so the desktop app
// could not resolve its own state directory and failed before it drew a window.
func TestEveryPathResolvesOnWindows(t *testing.T) {
	for name, fn := range map[string]func() (string, error){
		"StateDir":        paths.StateDir,
		"LogsDir":         paths.LogsDir,
		"LockfilePath":    paths.LockfilePath,
		"UpdateStatePath": paths.UpdateStatePath,
	} {
		got, err := fn()
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !filepath.IsAbs(got) {
			t.Errorf("%s = %q, want an absolute path", name, got)
		}
	}
}
