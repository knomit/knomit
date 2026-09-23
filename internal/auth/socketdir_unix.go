//go:build !windows

package auth

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
)

// SunPathCap is the size of a unix socket address's path field on this
// platform: 104 on darwin, 108 on linux. It is READ from the struct the
// kernel uses, never typed, so no platform's number can be baked into the
// other's build. A path of SunPathCap bytes or more is refused with
// ErrPathTooLong, which is exactly where bind(2) starts refusing on both
// platforms: TestListenLocal_PathLengthBoundaryIsTheSunPathCap asks the
// kernel on every run rather than trusting this function.
func SunPathCap() int { return len(syscall.RawSockaddrUnix{}.Path) }

// fallbackBase is where FallbackSocketDir lives. It is the LITERAL /tmp on
// every unix, never $TMPDIR, $XDG_RUNTIME_DIR or os.TempDir(): the bridge may
// run with a filtered environment, and a path derived from the environment
// would put the server's socket and the bridge's dial in different places
// with nothing to report it. A variable only so this package's tests can
// avoid the user's real directory.
var fallbackBase = "/tmp"

// socketDirOwner is the uid the fallback directory must belong to. A
// variable only so tests can stand in for "another user", which no
// unprivileged test can create.
var socketDirOwner = os.Geteuid

// FallbackSocketDir is the per-user directory for a local listener whose
// usual path under the data root is too long (knomit#253):
// /tmp/knomit-<euid>. /tmp is shared and world-writable, so the directory is
// used only after checkSocketDir says this user owns it with mode 0700.
func FallbackSocketDir() string {
	return filepath.Join(fallbackBase, "knomit-"+strconv.Itoa(os.Geteuid()))
}

// inFallbackDir reports whether path is a socket in FallbackSocketDir, the
// only place the ownership check applies. A data root is the operator's own
// directory and keeps today's behaviour whatever its mode.
func inFallbackDir(path string) bool {
	return filepath.Dir(filepath.Clean(path)) == FallbackSocketDir()
}

// checkSocketDir is the one test both sides apply to the fallback directory:
// a real directory (Lstat, so a symlink is refused, not followed), owned by
// socketDirOwner, with no group or other permission bits. A directory that
// does not exist returns the Lstat error, which wraps fs.ErrNotExist: to a
// dialler that means "no server here", not an attack.
func checkSocketDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&fs.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%w: %s is not a directory (mode %v)", ErrUnsafeSocketDir, dir, fi.Mode())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: %s: no owner information", ErrUnsafeSocketDir, dir)
	}
	if want := socketDirOwner(); int(st.Uid) != want {
		return fmt.Errorf("%w: %s is owned by uid %d, not %d", ErrUnsafeSocketDir, dir, st.Uid, want)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("%w: %s has mode %v, want 0700", ErrUnsafeSocketDir, dir, perm)
	}
	return nil
}

// prepareSocketDir runs before ListenLocal creates anything at path. Outside
// the fallback directory it does nothing. Inside it, it creates the directory
// 0700 if it is missing and then checks it: a directory someone else created
// first fails the check and nothing is created in it.
func prepareSocketDir(path string) error {
	if !inFallbackDir(path) {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("create socket dir: %w", err)
	}
	return checkSocketDir(dir)
}
