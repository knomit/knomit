//go:build !windows

package apppaths_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/apppaths"
)

// macOS and Linux keep ~/.knomit. The Windows default moved because Windows
// had not shipped; these have an installed base, and moving them is a separate
// decision with a migration attached.
func TestDefaultHome_IsDotKnomitInHome(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	got, err := apppaths.DefaultHome()
	if err != nil {
		t.Fatalf("DefaultHome: %v", err)
	}
	if want := filepath.Join(dir, ".knomit"); got != want {
		t.Errorf("DefaultHome = %q, want %q", got, want)
	}
}

// Same contract as the Windows case: an unresolvable home is an error and an
// empty string, never a rooted path built from "" that lands at the filesystem
// root.
func TestDefaultHome_ErrorsRatherThanReturningTheFilesystemRoot(t *testing.T) {
	t.Setenv("HOME", "")
	got, err := apppaths.DefaultHome()
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
	state, err := apppaths.StateDir()
	if err != nil {
		t.Skipf("no state dir on %s: %v", os.Getenv("GOOS"), err)
	}
	got, err := apppaths.LockfilePath()
	if err != nil {
		t.Fatalf("LockfilePath: %v", err)
	}
	if want := filepath.Join(state, "server.json"); got != want {
		t.Errorf("LockfilePath = %q, want %q", got, want)
	}
}
