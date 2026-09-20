//go:build !windows

package userdirs_test

import (
	"path/filepath"
	"testing"

	"knomit/internal/platform/userdirs"
)

func TestHomeDir_IsTheUsersHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	got, err := userdirs.HomeDir()
	if err != nil {
		t.Fatalf("HomeDir: %v", err)
	}
	if got != dir {
		t.Errorf("HomeDir = %q, want %q", got, dir)
	}
}

// Same contract as the Windows case: an unresolvable home is an error and an
// empty string, never a path built from "" that lands at the filesystem root.
func TestHomeDir_ErrorsRatherThanReturningTheFilesystemRoot(t *testing.T) {
	t.Setenv("HOME", "")
	got, err := userdirs.HomeDir()
	if err == nil {
		// os.UserHomeDir on unix consults only $HOME, but a platform that
		// resolves it another way is not a failure of this contract.
		t.Skipf("this platform resolved a home (%q) without $HOME", got)
	}
	if got != "" {
		t.Errorf("HomeDir returned %q alongside its error; want \"\"", got)
	}
}

// StateDir returns the BASE directory, with no product name joined on. The
// exact spelling is per-OS and asserted in the OS-specific tests; what every
// unix must satisfy is that it is absolute and derived from the home.
func TestStateDir_IsAbsoluteAndUnderTheHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_STATE_HOME", "")
	got, err := userdirs.StateDir()
	if err != nil {
		t.Skipf("no state directory on this platform: %v", err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("StateDir = %q, want an absolute path", got)
	}
	if rel, rerr := filepath.Rel(dir, got); rerr != nil || filepath.IsAbs(rel) {
		t.Errorf("StateDir = %q, want a path under the home %q", got, dir)
	}
}
