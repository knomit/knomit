//go:build !windows

package cmd

import (
	"os"
	"testing"
)

// widenRoot gives an existing data root the 0755 the earliest writers used
// to leave it at; t.TempDir() is 0700 already and would prove nothing.
func widenRoot(t *testing.T, home string) {
	t.Helper()
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertPrivateRoot(t *testing.T, home string) {
	t.Helper()
	fi, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if m := fi.Mode().Perm(); m != 0o700 {
		t.Errorf("data root after serve has mode %v, want 0700", m)
	}
}
