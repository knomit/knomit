package auth

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// shortSocketDir returns a fresh directory under /tmp: macOS caps sun_path at
// 104 bytes and t.TempDir() can exceed it, which would turn every dial into
// EINVAL and exercise a different branch from the one a test names.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no unix domain sockets on windows in this phase; named pipes are knomit/knomit#245")
	}
	dir, err := os.MkdirTemp("/tmp", "ll")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// makeStaleSocket leaves a socket FILE with nothing listening behind it — what
// a crashed server leaves. net.Listen unlinks on Close by default, which is
// what a crash does not do, so that is turned off first.
func makeStaleSocket(t *testing.T, path string) {
	t.Helper()
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	if st, err := os.Stat(path); err != nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected a stale SOCKET to remain at %s (err=%v)", path, err)
	}
	if _, err := net.Dial("unix", path); err == nil {
		t.Fatalf("fixture: stale socket at %s answered a dial", path)
	}
}

// statSocket stats path and fails unless it is a socket.
func statSocket(t *testing.T, path string) os.FileInfo {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("expected a socket at %s: %v %v", path, st, err)
	}
	return st
}

// acceptAll keeps a listener alive and answering until it is closed.
func acceptAll(ln net.Listener) {
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
}

func TestListenLocal_Mode0600_StaleRemoved_PeerCredOK(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "s.sock")
	if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil { // a stale REGULAR file
		t.Fatal(err)
	}
	ln, cleanup, err := ListenLocal(path)
	if err != nil || ln == nil {
		t.Fatalf("ListenLocal: %v %v", ln, err)
	}
	defer cleanup()
	st, err := os.Stat(path)
	if err != nil || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("not a socket after ListenLocal: %v %v", st, err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 0600", st.Mode().Perm())
	}
	acc := make(chan net.Conn, 1)
	go func() { c, _ := ln.Accept(); acc <- c }()
	cl, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	srv := <-acc
	defer srv.Close()
	uid, _, ok := PeerCred(srv)
	if !ok || uid != os.Getuid() {
		t.Fatalf("PeerCred over ListenLocal: uid=%d ok=%v", uid, ok)
	}
}

func TestListenLocal_CleanupUnlinks_AndIsIdempotent(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "s.sock")
	ln, cleanup, err := ListenLocal(path)
	if err != nil || ln == nil {
		t.Fatalf("ListenLocal: %v %v", ln, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("positive control: socket file must exist before cleanup: %v", err)
	}
	cleanup()
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket file must be gone after cleanup: %v", err)
	}
	// The lock file is NEVER unlinked: unlink-and-recreate would let two
	// processes each lock a different inode and both own the socket.
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("lock file must survive cleanup: %v", err)
	}
}

func TestListenLocal_LiveSocketIsNotStolen(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "s.sock")
	first, cleanup1, err := ListenLocal(path)
	if err != nil || first == nil {
		t.Fatalf("first ListenLocal: %v %v", first, err)
	}
	defer cleanup1()
	acceptAll(first)
	before := statSocket(t, path)
	second, cleanup2, err := ListenLocal(path)
	if !errors.Is(err, ErrSocketInUse) || second != nil {
		t.Fatalf("second ListenLocal on a LIVE socket must refuse with ErrSocketInUse: %v %v", second, err)
	}
	cleanup2() // the noop: must not unlink the first listener's file
	// A thief removes and recreates the SAME path, so existence proves nothing:
	// the file must be the very inode the first owner bound.
	if !os.SameFile(before, statSocket(t, path)) {
		t.Fatal("the live owner's socket was replaced by a different file")
	}
	// Positive control: the first listener still owns the path and still answers.
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("live socket was disturbed: %v", err)
	}
	c.Close()
}

func TestListenLocal_StaleSocketIsReplaced(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "s.sock")
	makeStaleSocket(t, path)
	// A crashed owner also leaves its lock file; with no holder it is inert.
	if err := os.WriteFile(path+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := statSocket(t, path)
	ln, cleanup, err := ListenLocal(path)
	if err != nil || ln == nil {
		t.Fatalf("stale socket must be replaced: %v %v", ln, err)
	}
	defer cleanup()
	// Positive control: the path now holds a NEW socket, not the leftover.
	if os.SameFile(stale, statSocket(t, path)) {
		t.Fatal("stale socket file was not replaced")
	}
	acceptAll(ln)
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("new listener not reachable: %v", err)
	}
	c.Close()
}

// The case a dial probe gets wrong: a live owner whose connects are REFUSED.
// A saturated accept backlog refuses exactly like a stale file, but its size
// is platform-specific (somaxconn 128 on darwin, 4096 on linux), so saturating
// it is not a fixture that holds everywhere. Instead the owner keeps its lock
// and closes its listener without unlinking: the file stays and every dial is
// refused with ECONNREFUSED — what a dial probe sees from a saturated server.
// Liveness must come from the lock, not from a dial.
func TestListenLocal_RefusingLiveOwnerIsNotStolen(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "s.sock")
	first, cleanup1, err := ListenLocal(path)
	if err != nil || first == nil {
		t.Fatalf("first ListenLocal: %v %v", first, err)
	}
	defer cleanup1() // the lock stays held until here
	first.(*net.UnixListener).SetUnlinkOnClose(false)
	first.Close()
	if _, err := net.Dial("unix", path); err == nil {
		t.Fatal("fixture: owner still answers; the test would not distinguish a dial probe")
	}
	before := statSocket(t, path)
	second, cleanup2, err := ListenLocal(path)
	if second != nil {
		defer second.Close()
	}
	if !errors.Is(err, ErrSocketInUse) || second != nil {
		t.Fatalf("a live owner that refuses connects must still be respected: %v %v", second, err)
	}
	cleanup2()
	if !os.SameFile(before, statSocket(t, path)) {
		t.Fatal("the live owner's socket was replaced by a different file")
	}
}

func TestListenLocal_EmptyPathIsNoop(t *testing.T) {
	ln, cleanup, err := ListenLocal("")
	if err != nil || ln != nil {
		t.Fatalf("empty path: %v %v", ln, err)
	}
	cleanup() // must not panic
}

// A lock that cannot be opened is a real failure, not ErrSocketInUse, and the
// message names the lock path once (os.PathError already carries it).
func TestListenLocal_UnopenableLockIsARealError(t *testing.T) {
	dir := shortSocketDir(t)
	if os.Getuid() == 0 {
		t.Skip("root ignores the 0500 mode this fixture relies on")
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	path := filepath.Join(dir, "s.sock")
	ln, cleanup, err := ListenLocal(path)
	defer cleanup()
	if err == nil || ln != nil || errors.Is(err, ErrSocketInUse) {
		t.Fatalf("unopenable lock must be a real error: %v %v", ln, err)
	}
	if n := strings.Count(err.Error(), path+".lock"); n != 1 {
		t.Fatalf("lock path named %d times, want 1: %v", n, err)
	}
}
