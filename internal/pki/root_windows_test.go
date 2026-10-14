package pki

import (
	"path/filepath"
	"testing"

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
