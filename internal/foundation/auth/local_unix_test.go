//go:build !windows

package auth

import (
	"os"
	"path/filepath"
	"testing"
)

// testLocalListenerPath is a path for one test's own listener. Under /tmp and
// not t.TempDir(): macOS caps sun_path at 104 bytes and t.TempDir() overruns
// it, which the phase 1 review found passing a fixture that proved nothing.
func testLocalListenerPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "al")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "l.sock")
}

// The stale-socket, 0600-mode and liveness fixtures for this platform live in
// listen_unix_test.go, which is where ListenLocal itself is tested. This file
// holds only what the identity half (local_unix.go) needs.
