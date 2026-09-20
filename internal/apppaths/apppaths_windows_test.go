//go:build windows

package apppaths_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/apppaths"
)

const testLocal = `C:\Users\test\AppData\Local`

func TestStateDir_UsesLocalAppData(t *testing.T) {
	t.Setenv("LOCALAPPDATA", testLocal)
	got, err := apppaths.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	if want := filepath.Join(testLocal, "knomit"); got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
}

// The data root is a level BELOW the state directory, not beside it. Beside it
// would put control.db, repos/, models/ and the keypair in the folder the
// desktop's "reveal log" menu item opens, next to server.json and the logs.
func TestDefaultHome_IsHomeSubdirOfStateDir(t *testing.T) {
	t.Setenv("LOCALAPPDATA", testLocal)
	state, err := apppaths.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	got, err := apppaths.DefaultHome()
	if err != nil {
		t.Fatalf("DefaultHome: %v", err)
	}
	if want := filepath.Join(state, "home"); got != want {
		t.Errorf("DefaultHome = %q, want %q", got, want)
	}
	if filepath.Dir(got) != state {
		t.Errorf("DefaultHome %q is not inside StateDir %q", got, state)
	}
}

// LOCALAPPDATA is set in any normal interactive session but can be absent
// under a service account or a stripped environment. Reconstructing the
// documented default beats failing while the information is still available.
func TestDefaultHome_ReconstructsWhenLocalAppDataUnset(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no user profile on this machine: %v", err)
	}
	got, err := apppaths.DefaultHome()
	if err != nil {
		t.Fatalf("DefaultHome: %v", err)
	}
	if want := filepath.Join(home, "AppData", "Local", "knomit", "home"); got != want {
		t.Errorf("DefaultHome = %q, want %q", got, want)
	}
}

// THE REGRESSION. With neither variable set, the old code produced "/.knomit",
// which Windows resolves against the current drive as C:\.knomit — a writable,
// plausible-looking directory that a real aborted launch actually populated
// with an SSH keypair and a partial model download.
//
// Two separate assertions on purpose: that an error is returned, and that
// nothing path-shaped comes back with it. A future refactor that returns a
// "best effort" path alongside a non-nil error would satisfy the first and
// reintroduce the bug for any caller that logs the error and continues.
func TestDefaultHome_ErrorsRatherThanReturningADriveRoot(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOME", "") // ignored by os.UserHomeDir on Windows; scrubbed anyway

	got, err := apppaths.DefaultHome()
	if err == nil {
		t.Fatalf("DefaultHome = %q with no error; want an error when nothing resolves", got)
	}
	if got != "" {
		t.Errorf("DefaultHome returned %q alongside its error; want \"\"", got)
	}
	// The message has to tell the operator what to do about it.
	if !strings.Contains(err.Error(), "KNOMIT_HOME") {
		t.Errorf("error %q does not mention KNOMIT_HOME, the documented way out", err)
	}
}

func TestLockfilePath_IsServerJSONInStateDir(t *testing.T) {
	t.Setenv("LOCALAPPDATA", testLocal)
	state, err := apppaths.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	got, err := apppaths.LockfilePath()
	if err != nil {
		t.Fatalf("LockfilePath: %v", err)
	}
	if want := filepath.Join(state, "server.json"); got != want {
		t.Errorf("LockfilePath = %q, want %q", got, want)
	}
}
