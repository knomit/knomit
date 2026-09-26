//go:build windows

package app

import (
	"path/filepath"
	"testing"

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
	if !in.Protected || len(in.ACEs) != 2 {
		t.Errorf("data root after New is not a protected two-ACE DACL: %+v", in)
	}
}
