package pki

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The cross-process file lock (flock on POSIX, LockFileEx on Windows). It
// lives here because InstallBundle needs it and pki imports no knomit
// package; the bridge's credentials lock (tools/bridge/knomitapi) uses the
// same two functions rather than a copy.

// lockPollInterval is how often a waiter retries a held lock. A latency
// choice, not a measured property of anything.
const lockPollInterval = 25 * time.Millisecond

// pollLock calls try until it takes the lock, fails, or ctx ends.
func pollLock(ctx context.Context, try func() (bool, error)) error {
	t := time.NewTicker(lockPollInterval)
	defer t.Stop()
	for {
		ok, err := try()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// installLockWait bounds how long InstallBundle waits for another install
// (another process: the CLI, or the desktop) to finish. An install is a few
// file writes, so a wait this long means the holder is stuck; it is a
// latency choice, not a measured property of anything.
const installLockWait = 10 * time.Second

// LockPath is the file InstallBundle locks for the pki dir dir: a SIBLING of
// dir, "<dir>.lock", never a file inside it — so a refused install still
// leaves dir exactly as it was (on a fresh home, absent). Only dir's parent
// is created, 0700, when missing. So dir's PARENT must be writable: a pki dir
// made ahead of time inside a directory the installing user cannot write
// (/etc/knomit/pki, say) now fails with "install lock <path>: ...". The lock file is left in place: removing
// it would let a waiter lock an unlinked inode while a third process locks
// a new one.
func LockPath(dir string) string { return filepath.Clean(dir) + ".lock" }

// lockInstall takes the install lock for dir and returns its release.
func lockInstall(dir string) (func(), error) {
	p := LockPath(dir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("install lock %s: %w", p, err)
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("install lock %s (its directory must be writable): %w", p, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), installLockWait)
	defer cancel()
	if err := LockFile(ctx, f); err != nil {
		f.Close()
		if ctx.Err() != nil {
			return nil, fmt.Errorf("install lock %s: waited %s for another identity install to finish: %w", p, installLockWait, err)
		}
		return nil, fmt.Errorf("install lock %s: %w", p, err)
	}
	return func() {
		_ = UnlockFile(f)
		f.Close()
	}, nil
}
