//go:build !windows

package auth

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// These are the UNIX half of the ListenLocal tests. The file is build-tagged
// rather than skipped from inside, because every fixture below is about a
// socket FILE: the 0600 mode, the stale inode a crash leaves, the flock that
// decides liveness. None of them has a meaning on Windows, where the pipe
// namespace does that job and nothing outlives the process. The Windows half
// -- the same QUESTIONS, asked of a pipe -- is listen_windows_test.go.
//
// shortSocketDir returns a fresh directory under /tmp: macOS caps sun_path at
// 104 bytes and t.TempDir() can exceed it, which would turn every dial into
// EINVAL and exercise a different branch from the one a test names.
func shortSocketDir(t *testing.T) string {
	t.Helper()
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
	peer, ok := PeerCred(srv)
	if !ok || peer.ID != localID(os.Getuid()) {
		t.Fatalf("PeerCred over ListenLocal: id=%q ok=%v, want %q", peer.ID, ok, localID(os.Getuid()))
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
	// Precondition: the leftover refuses, which is what makes it stale.
	if _, err := net.Dial("unix", path); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("fixture: stale socket must refuse with ECONNREFUSED, got %v", err)
	}
	ln, cleanup, err := ListenLocal(path)
	if err != nil || ln == nil {
		t.Fatalf("stale socket must be replaced: %v %v", ln, err)
	}
	defer cleanup()
	// "Replaced" is checked by BEHAVIOUR, not by inode: ext4 hands the inode
	// freed by the removal straight to the new socket, so os.SameFile reports
	// the same file for a genuinely new one. A dial that the NEW listener's
	// Accept receives, with the kernel's peer credential on it, is the proof.
	acc := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			c = nil
		}
		acc <- c
	}()
	cl, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("new listener not reachable: %v", err)
	}
	defer cl.Close()
	var srv net.Conn
	select {
	case srv = <-acc:
	case <-time.After(2 * time.Second):
		t.Fatal("the dial was not received by the new listener's Accept")
	}
	if srv == nil {
		t.Fatal("new listener's Accept failed")
	}
	defer srv.Close()
	if peer, ok := PeerCred(srv); !ok || peer.ID != localID(os.Getuid()) {
		t.Fatalf("PeerCred on the replaced socket: id=%q ok=%v, want %q", peer.ID, ok, localID(os.Getuid()))
	}
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

// --- knomit#253: sun_path length and the fallback directory ---

// capPath returns a path of EXACTLY n bytes inside a fresh short directory.
// n is always derived from SunPathCap, never typed, so the test means the
// same thing on darwin (104) and linux (108).
func capPath(t *testing.T, n int) string {
	t.Helper()
	dir := shortSocketDir(t)
	pad := n - len(dir) - 1 - len(".sock")
	if pad < 1 {
		t.Fatalf("short dir %q leaves no room for a %d-byte path", dir, n)
	}
	p := filepath.Join(dir, strings.Repeat("s", pad)+".sock")
	if len(p) != n {
		t.Fatalf("built a %d-byte path, want %d", len(p), n)
	}
	return p
}

func assertNothingCreated(t *testing.T, path string) {
	t.Helper()
	for _, p := range []string{path, path + ".lock"} {
		// ENOTDIR is also "absent": the parent is a file, so nothing can be
		// under it.
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			t.Errorf("%s exists after a refused ListenLocal (err=%v)", p, err)
		}
	}
}

// E5: the boundary itself. cap-1 bytes listens and dials; cap bytes is the
// named error BEFORE any file is created. Sabotage: typing 108 for the cap
// passes on linux and fails the second half on darwin, and vice versa for
// 104 — the reviewer runs the linux half in a container.
func TestListenLocal_PathLengthBoundaryIsTheSunPathCap(t *testing.T) {
	under := capPath(t, SunPathCap()-1)
	ln, cleanup, err := ListenLocal(under)
	if err != nil {
		t.Fatalf("ListenLocal(%d bytes, cap %d): %v", len(under), SunPathCap(), err)
	}
	defer cleanup()
	go acceptAll(ln)
	c, err := DialLocal(t.Context(), under, time.Second)
	if err != nil {
		t.Fatalf("dial the cap-1 socket: %v", err)
	}
	c.Close()

	at := capPath(t, SunPathCap())
	_, cleanup2, err := ListenLocal(at)
	defer cleanup2()
	if !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("ListenLocal(%d bytes) = %v, want ErrPathTooLong", len(at), err)
	}
	if errors.Is(err, syscall.EINVAL) {
		t.Fatalf("the refusal is a raw EINVAL from net.Listen, not the pre-check: %v", err)
	}
	assertNothingCreated(t, at)
}

func TestListenLocal_120ByteHomeIsErrPathTooLongNotEINVAL(t *testing.T) {
	p := filepath.Join("/tmp", strings.Repeat("h", 120), "knomit.sock")
	_, cleanup, err := ListenLocal(p)
	defer cleanup()
	if !errors.Is(err, ErrPathTooLong) {
		t.Fatalf("ListenLocal(%d bytes) = %v, want ErrPathTooLong", len(p), err)
	}
	if !strings.Contains(err.Error(), p) {
		t.Errorf("error %q does not name the path", err)
	}
	assertNothingCreated(t, p)
}

// withFallbackBase points the fallback directory at a private test base for
// the test's duration, so no test touches the user's real /tmp/knomit-<uid>
// (a running desktop may be listening there).
func withFallbackBase(t *testing.T) string {
	t.Helper()
	base := shortSocketDir(t)
	prev := fallbackBase
	fallbackBase = base
	t.Cleanup(func() { fallbackBase = prev })
	return base
}

func TestFallbackSocketDir_IsLiteralTmpPerEUID(t *testing.T) {
	want := filepath.Join("/tmp", "knomit-"+strconv.Itoa(os.Geteuid()))
	if got := FallbackSocketDir(); got != want {
		t.Fatalf("FallbackSocketDir() = %q, want %q", got, want)
	}
}

func TestListenLocal_FallbackDirIsCreatedPrivate(t *testing.T) {
	withFallbackBase(t)
	p := filepath.Join(FallbackSocketDir(), "abcdef01.sock")
	ln, cleanup, err := ListenLocal(p)
	if err != nil {
		t.Fatalf("ListenLocal in a fresh fallback dir: %v", err)
	}
	defer cleanup()
	go acceptAll(ln)
	fi, err := os.Lstat(FallbackSocketDir())
	if err != nil {
		t.Fatal(err)
	}
	if !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("fallback dir mode %v, want a 0700 directory", fi.Mode())
	}
	c, err := DialLocal(t.Context(), p, time.Second)
	if err != nil {
		t.Fatalf("dial through the checked fallback dir: %v", err)
	}
	c.Close()
}

// D7: a fallback directory the current user does not own with mode 0700 is
// never used: the named error, and no socket or lock file inside it.
func TestListenLocal_UnsafeFallbackDirIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"group-and-world-readable", func(t *testing.T, dir string) {
			if err := os.Mkdir(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			os.Chmod(dir, 0o755) // defeat the umask
		}},
		{"owned-by-another-uid", func(t *testing.T, dir string) {
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			// Nobody but root can chown to another uid, so the test moves
			// who "we" are instead: the check must compare against this.
			prev := socketDirOwner
			socketDirOwner = func() int { return os.Geteuid() + 1 }
			t.Cleanup(func() { socketDirOwner = prev })
		}},
		{"symlink-to-a-private-dir", func(t *testing.T, dir string) {
			target := shortSocketDir(t) // 0700, ours: only the link is wrong
			if err := os.Symlink(target, dir); err != nil {
				t.Fatal(err)
			}
		}},
		{"a-file-not-a-dir", func(t *testing.T, dir string) {
			if err := os.WriteFile(dir, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withFallbackBase(t)
			dir := FallbackSocketDir()
			tc.setup(t, dir)
			p := filepath.Join(dir, "abcdef01.sock")
			_, cleanup, err := ListenLocal(p)
			defer cleanup()
			if !errors.Is(err, ErrUnsafeSocketDir) {
				t.Fatalf("ListenLocal = %v, want ErrUnsafeSocketDir", err)
			}
			if _, err := DialLocal(t.Context(), p, time.Second); !errors.Is(err, ErrUnsafeSocketDir) {
				t.Fatalf("DialLocal = %v, want ErrUnsafeSocketDir: the bridge must not dial into it either", err)
			}
			assertNothingCreated(t, p)
		})
	}
}

// A missing fallback dir is "no server here", the bridge's quiet case, not a
// security refusal.
func TestDialLocal_MissingFallbackDirIsNotExist(t *testing.T) {
	withFallbackBase(t)
	_, err := DialLocal(t.Context(), filepath.Join(FallbackSocketDir(), "abcdef01.sock"), time.Second)
	if !errors.Is(err, os.ErrNotExist) || errors.Is(err, ErrUnsafeSocketDir) {
		t.Fatalf("DialLocal into a missing fallback dir = %v, want fs.ErrNotExist only", err)
	}
}

// Scope: the ownership check is for the SHARED fallback base only. A data
// root is the operator's own directory and keeps today's behaviour, whatever
// its mode.
func TestListenLocal_OrdinaryDirIsNotOwnershipChecked(t *testing.T) {
	withFallbackBase(t)
	dir := shortSocketDir(t)
	os.Chmod(dir, 0o755)
	ln, cleanup, err := ListenLocal(filepath.Join(dir, "knomit.sock"))
	if err != nil {
		t.Fatalf("ListenLocal in a 0755 non-fallback dir: %v", err)
	}
	defer cleanup()
	ln.Close()
}
