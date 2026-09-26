//go:build windows

package app

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"

	"knomit/internal/platform/privdir"
)

// wideHome is a data root that does not exist yet, under t.TempDir(). What it
// would get without privdir is the parent's INHERITED, unprotected DACL, so
// asserting Protected afterwards is not vacuous even under the profile.
func wideHome(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "home")
}

func assertPrivateRoot(t *testing.T, home string) {
	t.Helper()
	in, err := privdir.Inspect(home)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Protected || in.NullDACL || len(in.ACEs) != 2 {
		t.Fatalf("data root after New is not a protected two-ACE DACL: %+v", in)
	}
	u, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	// The same shape as privdir's assertPrivateDir: one allow FA ACE each for
	// the current user and SYSTEM, not inherited, flags exactly OI|CI.
	want := map[string]bool{u.User.Sid.String(): true, "S-1-5-18": true}
	for _, a := range in.ACEs {
		if !want[a.SID] || !a.Allow || a.Mask != 0x1f01ff || a.Inherited ||
			a.Flags != windows.OBJECT_INHERIT_ACE|windows.CONTAINER_INHERIT_ACE {
			t.Errorf("data root after New: unexpected or repeated ACE %+v", a)
		}
		delete(want, a.SID)
	}
}
