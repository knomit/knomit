package auth

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"runtime"
	"sync"
)

// ErrSocketInUse means another knomit instance is ALIVE on the socket path.
// The caller must not fail on it: log at WARN and serve TCP only. Taking the
// path over would silently redirect every bridge away from the instance that
// was there first — after phase 1c both `knomit serve` and the desktop app
// open the same path.
var ErrSocketInUse = errors.New("local socket is in use by a live knomit instance")

// errLockHeld is what lockExclusive returns when another open file
// description already holds the lock.
var errLockHeld = errors.New("lock held")

// ListenLocal is the ONE place the local authenticated listener is opened.
// Both binaries that serve knomit — `knomit serve` (cmd/serve.go) and the
// desktop app (tools/desktop/boot.go) — call it, because the desktop builds
// its own http.Server and does not pass through cmd/serve.go: phase 1 put the
// socket in cmd/serve.go only, and a desktop-served machine never opened one
// (issue #248).
//
// 0600 on the socket, under the 0700 data root, IS the credential: the kernel
// vouches for the peer uid (PeerCred) and the mode decides which uids can
// reach the socket at all.
//
// Live or stale is NOT answered by dialing the socket: a live listener whose
// accept backlog is full refuses with ECONNREFUSED exactly like a stale file,
// so a probe would steal a busy server's socket. Instead the owner holds an
// exclusive flock on <path>.lock for the life of the listener. The kernel
// releases it on ANY exit, graceful or not, so "lock held" is exactly "owner
// alive" (ErrSocketInUse, nothing touched) and "lock acquired" is exactly
// "whatever sits at path is a leftover", which is removed.
//
// The returned cleanup closes the listener, unlinks the socket and releases
// the lock; it is safe to call twice. It keeps the lock file reachable, so the
// caller must hold on to it for the listener's life: a dropped cleanup lets
// the GC finalizer close the descriptor and release the lock early.
//
// path == "" or a platform without unix sockets returns (nil, noop, nil), so
// callers need no platform branch. Issue #245 adds the Windows named pipe
// HERE, not in the callers.
func ListenLocal(path string) (net.Listener, func(), error) {
	noop := func() {}
	if path == "" || runtime.GOOS == "windows" {
		return nil, noop, nil
	}
	lockPath := path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, noop, fmt.Errorf("open socket lock %s: %w", lockPath, err)
	}
	if err := lockExclusive(lockFile); err != nil {
		lockFile.Close()
		if errors.Is(err, errLockHeld) {
			return nil, noop, fmt.Errorf("%w: %s", ErrSocketInUse, path)
		}
		return nil, noop, fmt.Errorf("lock %s: %w", lockPath, err)
	}
	// From here every failure closes lockFile, which releases the lock.
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		lockFile.Close()
		return nil, noop, fmt.Errorf("remove stale socket %s: %w", path, err)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		lockFile.Close()
		return nil, noop, fmt.Errorf("unix socket listen %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		os.Remove(path)
		lockFile.Close()
		return nil, noop, fmt.Errorf("chmod socket %s: %w", path, err)
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			ln.Close()
			os.Remove(path)
			// Release the lock LAST, after the socket is gone, so a successor
			// can never have its fresh socket unlinked by us. The .lock file
			// itself is never unlinked: unlink-and-recreate would let two
			// processes each lock a different inode and both own the socket.
			lockFile.Close()
		})
	}
	return ln, cleanup, nil
}
