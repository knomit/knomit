//go:build windows

package userdirs_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/platform/userdirs"
)

const testLocal = `C:\Users\test\AppData\Local`

func TestStateDir_IsLocalAppDataItself(t *testing.T) {
	t.Setenv("LOCALAPPDATA", testLocal)
	got, err := userdirs.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	// The BASE, with nothing joined on. Anything appended here would be a
	// product name, which this tier does not know.
	if got != testLocal {
		t.Errorf("StateDir = %q, want the bare %q", got, testLocal)
	}
}

// LOCALAPPDATA is set in any normal interactive session but can be absent
// under a service account or a stripped environment. Reconstructing the
// documented default beats failing while the information is still available.
func TestStateDir_ReconstructsWhenLocalAppDataUnset(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no user profile on this machine: %v", err)
	}
	got, err := userdirs.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	if want := filepath.Join(home, "AppData", "Local"); got != want {
		t.Errorf("StateDir = %q, want the reconstructed %q", got, want)
	}
}

// An empty-but-SET variable counts as unset. Returning "" here would make the
// caller's Join produce a RELATIVE path, which is the same class of bug as the
// drive-root one below rather than a tidier one.
func TestStateDir_TreatsEmptyLocalAppDataAsUnset(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	if _, err := os.UserHomeDir(); err != nil {
		t.Skipf("no user profile on this machine: %v", err)
	}
	got, err := userdirs.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	if got == "" || !filepath.IsAbs(got) {
		t.Errorf("StateDir = %q, want an absolute path rather than something Join would make relative", got)
	}
}

// THE REGRESSION, at the tier that owns it. With neither variable set the old
// code produced a root-relative path, which Windows resolves against the
// CURRENT DRIVE — a writable, plausible-looking directory that a real aborted
// launch actually populated.
//
// Two separate assertions on purpose: that an error is returned, and that
// nothing path-shaped comes back with it. A future refactor returning a "best
// effort" path alongside a non-nil error would satisfy the first and
// reintroduce the bug for any caller that logs the error and continues.
func TestStateDir_ErrorsRatherThanReturningADriveRoot(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOME", "") // ignored by os.UserHomeDir on Windows; scrubbed anyway

	got, err := userdirs.StateDir()
	if err == nil {
		t.Fatalf("StateDir = %q with no error; want an error when nothing resolves", got)
	}
	if got != "" {
		t.Errorf("StateDir returned %q alongside its error; want \"\"", got)
	}
	// The message has to name what an operator can actually set. It must NOT
	// name an application's own override — this tier does not know there is one.
	if !strings.Contains(err.Error(), "LOCALAPPDATA") {
		t.Errorf("error %q does not name the variable that was missing", err)
	}
}

func TestHomeDir_ErrorsRatherThanReturningEmpty(t *testing.T) {
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOME", "")
	got, err := userdirs.HomeDir()
	if err == nil {
		t.Skipf("this platform resolved a profile (%q) without %%USERPROFILE%%", got)
	}
	if got != "" {
		t.Errorf("HomeDir returned %q alongside its error; want \"\"", got)
	}
}
