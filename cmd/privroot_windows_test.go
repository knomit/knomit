//go:build windows

package cmd

import (
	"testing"

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
	if !in.Protected || len(in.ACEs) != 2 {
		t.Errorf("data root after serve is not a protected two-ACE DACL: %+v", in)
	}
}
