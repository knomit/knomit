package modroot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDir_IsTheModuleRoot(t *testing.T) {
	root := Dir(t)
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("Dir(t) = %q has no go.mod: %v", root, err)
	}
	if _, err := os.Stat(Path(t, "internal", "testsupport", "modroot", "modroot.go")); err != nil {
		t.Fatalf("Path does not resolve from the root: %v", err)
	}
}

func TestFind_FailsWithoutGoMod(t *testing.T) {
	dir := t.TempDir()
	if root, err := Find(dir); err == nil {
		// A go.mod above the temp dir would make this vacuous; say so.
		t.Fatalf("Find(%q) = %q, want an error (no go.mod above a temp dir)", dir, root)
	}
}
