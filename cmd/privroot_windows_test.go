//go:build windows

package cmd

import (
	"testing"

	"golang.org/x/sys/windows"

	"knomit/internal/platform/privdir"
)

// widenRoot is a no-op on Windows: a t.TempDir() carries its parent's
// INHERITED, unprotected DACL, which is what privdir replaces, so asserting
// Protected afterwards is not vacuous.
func widenRoot(t *testing.T, home string) { t.Helper() }

func assertPrivateRoot(t *testing.T, home string) {
	t.Helper()
	in, err := privdir.Inspect(home)
	if err != nil {
		t.Fatal(err)
	}
	if !in.Protected || in.NullDACL || len(in.ACEs) != 2 {
		t.Fatalf("data root after serve is not a protected two-ACE DACL: %+v", in)
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
			t.Errorf("data root after serve: unexpected or repeated ACE %+v", a)
		}
		delete(want, a.SID)
	}
}
