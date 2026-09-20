//go:build windows

package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"knomit/internal/config"
	"knomit/internal/platform/userdirs"
)

const testLocal = `C:\Users\test\AppData\Local`

// THE NAME PIN. The OS resolution itself is internal/platform/userdirs'
// business and is tested there; what config owns is that the knomit folder is
// joined onto whatever userdirs answered, spelled exactly once.
//
// Deriving `want` from userdirs rather than restating the path is deliberate: a
// test that hardcodes both halves still passes when the two sides disagree.
func TestStateDir_IsTheOSStateDirPlusTheAppName(t *testing.T) {
	t.Setenv("LOCALAPPDATA", testLocal)
	base, err := userdirs.StateDir()
	if err != nil {
		t.Fatalf("userdirs.StateDir: %v", err)
	}
	got, err := config.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	if want := filepath.Join(base, "knomit"); got != want {
		t.Errorf("StateDir = %q, want %q", got, want)
	}
}

// The data root is a level BELOW the state directory, not beside it. Beside it
// would put control.db, repos/, models/ and the keypair in the folder the
// desktop's "reveal log" menu item opens, next to server.json and the logs.
func TestDefaultHome_IsHomeSubdirOfStateDir(t *testing.T) {
	t.Setenv("LOCALAPPDATA", testLocal)
	state, err := config.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	got, err := config.DefaultHome()
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

// End to end through both packages. The reconstruction lives in userdirs, but
// what a knomit binary actually resolves to is this one's answer, and that is
// what the C:\.knomit incident was about.
func TestDefaultHome_ReconstructsWhenLocalAppDataUnset(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no user profile on this machine: %v", err)
	}
	got, err := config.DefaultHome()
	if err != nil {
		t.Fatalf("DefaultHome: %v", err)
	}
	if want := filepath.Join(home, "AppData", "Local", "knomit", "home"); got != want {
		t.Errorf("DefaultHome = %q, want %q", got, want)
	}
}

// The APPLICATION half of the never-guess rule: userdirs refuses to invent a
// directory, and config must turn that refusal into a message naming the
// override an operator can act on. userdirs cannot say KNOMIT_HOME — it does
// not know this application has one — so if the wrapping is dropped the error
// degrades to "%LOCALAPPDATA% is unset" with no way out.
//
// Two separate assertions on purpose: that an error is returned, and that
// nothing path-shaped comes back with it. A "best effort" path alongside a
// non-nil error would satisfy the first and reintroduce C:\.knomit for any
// caller that logs the error and continues.
func TestDefaultHome_ErrorsRatherThanReturningADriveRoot(t *testing.T) {
	t.Setenv("LOCALAPPDATA", "")
	t.Setenv("USERPROFILE", "")
	t.Setenv("HOME", "") // ignored by os.UserHomeDir on Windows; scrubbed anyway

	got, err := config.DefaultHome()
	if err == nil {
		t.Fatalf("DefaultHome = %q with no error; want an error when nothing resolves", got)
	}
	if got != "" {
		t.Errorf("DefaultHome returned %q alongside its error; want \"\"", got)
	}
	if !strings.Contains(err.Error(), "KNOMIT_HOME") {
		t.Errorf("error %q does not mention KNOMIT_HOME, the documented way out", err)
	}
}

func TestLockfilePath_IsServerJSONInStateDir(t *testing.T) {
	t.Setenv("LOCALAPPDATA", testLocal)
	state, err := config.StateDir()
	if err != nil {
		t.Fatalf("StateDir: %v", err)
	}
	got, err := config.LockfilePath()
	if err != nil {
		t.Fatalf("LockfilePath: %v", err)
	}
	if want := filepath.Join(state, "server.json"); got != want {
		t.Errorf("LockfilePath = %q, want %q", got, want)
	}
}
