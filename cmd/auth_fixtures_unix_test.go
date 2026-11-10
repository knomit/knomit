//go:build !windows

package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// authLocalListenerPath is a local listener path this platform can open:
// under /tmp, because macOS caps sun_path at 104 bytes and t.TempDir()
// overruns it.
func authLocalListenerPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "ka")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "knomit.sock")
}
