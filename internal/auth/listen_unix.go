//go:build !windows

package auth

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"sync"
	"syscall"
)

// errLockHeld is what lockExclusive returns when another open file
// description already holds the lock.
var errLockHeld = errors.New("lock held")

// listenLocal opens the unix half of ListenLocal: see that doc for why the
// lock file decides liveness and why 0600 under a 0700 root is the credential.
func listenLocal(path string) (net.Listener, func(), error) {
	noop := func() {}
	// Length first, before the lock file or anything else exists: a path
	// net.Listen cannot bind is refused by name, never as net.Listen's
	// EINVAL after a .lock was already created beside it.
	if len(path) >= SunPathCap() {
		return nil, noop, fmt.Errorf("%w: %s is %d bytes, the cap on this platform is %d", ErrPathTooLong, path, len(path), SunPathCap())
	}
	if err := prepareSocketDir(path); err != nil {
		return nil, noop, err
	}
	lockPath := path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, noop, fmt.Errorf("open socket lock: %w", err) // PathError already names lockPath
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

// lockExclusive takes a non-blocking exclusive flock on f. flock, not fcntl:
// flock locks belong to the open file description, so a second ListenLocal in
// the SAME process is refused too; fcntl locks are per process and would let
// it through.
func lockExclusive(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return errLockHeld
	}
	return err
}
