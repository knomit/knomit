//go:build !windows

package authz

import (
	"os"
	"path/filepath"
	"testing"
)

// testLocalListenerPath is a socket path for one test's own listener. Under
// /tmp and not t.TempDir(): macOS caps sun_path at 104 bytes and t.TempDir()
// overruns it (same reason as auth's helper of the same name).
func testLocalListenerPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "az")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "l.sock")
}
