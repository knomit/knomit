//go:build !windows

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
)

// macOS and Linux keep ~/.knomit. The Windows default moved because Windows had
// not shipped; these have an installed base, and moving them is a separate
// decision with a migration attached.
//
// This pins both halves config owns: the dotted NAME, and the POLICY that it
// hangs off the home directory rather than off the OS state directory as on
// Windows.
func TestDefaultHome_IsDotKnomitInHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	got, err := config.DefaultHome()
	if err != nil {
		t.Fatalf("DefaultHome: %v", err)
	}
	if want := filepath.Join(dir, ".knomit"); got != want {
		t.Errorf("DefaultHome = %q, want %q", got, want)
	}
}

// Same contract as the Windows twin: the OS refusal is userdirs', naming
// KNOMIT_HOME is config's, and an unresolvable home is an error AND an empty
// string — never a path built from "" that lands at the filesystem root.
func TestDefaultHome_ErrorsRatherThanReturningTheFilesystemRoot(t *testing.T) {
	t.Setenv("HOME", "")
	got, err := config.DefaultHome()
	if err == nil {
		// os.UserHomeDir on unix consults only $HOME, but a platform that
		// resolves it another way is not a failure of this contract.
		if got == filepath.Join("", ".knomit") || got == "/.knomit" {
			t.Fatalf("DefaultHome = %q — a root-relative path built from an empty home", got)
		}
		t.Skipf("this platform resolved a home (%q) without $HOME", got)
	}
	if got != "" {
		t.Errorf("DefaultHome returned %q alongside its error; want \"\"", got)
	}
	if !strings.Contains(err.Error(), "KNOMIT_HOME") {
		t.Errorf("error %q does not mention KNOMIT_HOME, the documented way out", err)
	}
}

func TestLockfilePath_IsServerJSONInStateDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state, err := config.StateDir()
	if err != nil {
		t.Skipf("no state dir on %s: %v", os.Getenv("GOOS"), err)
	}
	got, err := config.LockfilePath()
	if err != nil {
		t.Fatalf("LockfilePath: %v", err)
	}
	if want := filepath.Join(state, "server.json"); got != want {
		t.Errorf("LockfilePath = %q, want %q", got, want)
	}
}
