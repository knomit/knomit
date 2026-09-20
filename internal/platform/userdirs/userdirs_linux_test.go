//go:build linux

package userdirs_test

import (
	"path/filepath"
	"testing"

	"knomit/internal/platform/userdirs"
)

func TestStateDir_HonoursXDGStateHome(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	got, err := userdirs.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	if got != xdg {
		t.Errorf("StateDir = %q, want the bare %q", got, xdg)
	}
}

func TestStateDir_FallsBackToLocalState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	got, err := userdirs.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	if want := filepath.Join(home, ".local", "state"); got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
}

// Empty counts as unset, per the XDG spec. Returning "" would hand the caller
// a value whose Join is RELATIVE — resolved against the working directory.
func TestStateDir_TreatsEmptyXDGAsUnset(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	got, err := userdirs.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("StateDir = %q, want an absolute path", got)
	}
}
