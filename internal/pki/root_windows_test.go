package pki

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"knomit/internal/platform/privdir"
)

// On Windows the mode bits say nothing (every file reads 0666), so the
// owner-only property of the fleet root directory is its DACL: NewRoot makes
// the directory a PROTECTED DACL through privdir.Ensure, and root.key inside
// it carries only inherited ACEs, none for BUILTIN\Users. A directory made
// with os.MkdirAll is not protected, so this fails if NewRoot stops calling
// privdir.Ensure.
func TestNewRoot_FleetRootDirIsAProtectedDACL(t *testing.T) {
	dir, _ := newTestRoot(t)
	in, err := privdir.Inspect(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Protected || in.NullDACL {
		t.Fatalf("%s: DACL not protected: %+v", dir, in)
	}
	key, err := privdir.Inspect(filepath.Join(dir, RootKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range key.ACEs {
		if !a.Inherited || a.SID == "S-1-5-32-545" {
			t.Fatalf("root.key carries an ACE beyond the private directory's: %+v", a)
		}
	}
}

// grantUsers gives path a PROTECTED DACL that also grants BUILTIN\Users
// read+execute, inheritable: the shape privdir.Ensure leaves alone (it was
// protected on purpose, perhaps by another local user) and only warns about.
func grantUsers(t *testing.T, path string) {
	t.Helper()
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;" + u.User.Sid.String() + ")(A;OICI;FA;;;SY)(A;OICI;0x1200a9;;;BU)")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// NewRoot refuses a fleet root directory whose protected DACL grants
// BUILTIN\Users, and writes no key into it: on Windows the directory's DACL
// is the key's only protection.
func TestNewRoot_RefusesADirWhoseDACLGrantsOthers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fleet")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	grantUsers(t, dir)
	_, err := NewRoot(dir, "knomit-fleet-root-test", time.Hour)
	if !errors.Is(err, privdir.ErrNotPrivate) || !strings.Contains(err.Error(), "S-1-5-32-545") {
		t.Fatalf("NewRoot into a DACL granting Users: %v, want privdir.ErrNotPrivate naming S-1-5-32-545", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, RootKeyFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused NewRoot wrote root.key: %v", err)
	}
}

// LoadRoot refuses the same shape: a fleet directory copied to (or
// re-permissioned in) a place that grants others.
func TestLoadRoot_RefusesADirWhoseDACLGrantsOthers(t *testing.T) {
	dir, _ := newTestRoot(t)
	if _, err := LoadRoot(dir); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	grantUsers(t, dir)
	if _, err := LoadRoot(dir); !errors.Is(err, privdir.ErrNotPrivate) {
		t.Fatalf("LoadRoot from a DACL granting Users: %v, want privdir.ErrNotPrivate", err)
	}
}
