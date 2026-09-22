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

// A socket file left by an ungraceful exit must not stop the next boot: the
// listener removes it. Without this, a crashed server would need manual
// cleanup before it could start again.
func TestListenLocal_ReplacesAStaleSocket(t *testing.T) {
	path := testLocalListenerPath(t)
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := ListenLocal(path)
	if err != nil {
		t.Fatalf("a stale file at the socket path must not stop the boot: %v", err)
	}
	defer l.Close()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("mode = %v, want a socket", st.Mode())
	}
	// 0600 under the 0700 data root IS the credential: a world-writable
	// socket would let any local user be taken for this one.
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode = %#o, want 0600", perm)
	}
}
