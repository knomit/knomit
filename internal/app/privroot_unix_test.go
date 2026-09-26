//go:build !windows

package app

import (
	"os"
	"path/filepath"
	"testing"
)

// wideHome is an existing data root at 0755, the mode the earliest writers
// used to leave it at. A t.TempDir() is already 0700 and would prove nothing.
func wideHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	return home
}

func assertPrivateRoot(t *testing.T, home string) {
	t.Helper()
	fi, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o700 {
		t.Errorf("data root after New has mode %v, want 0700", m)
	}
}
