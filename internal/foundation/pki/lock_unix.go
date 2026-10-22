//go:build !windows

package pki

import (
	"context"
	"errors"
	"os"
	"syscall"
)

// LockFile takes an exclusive flock on f, waiting for a holder but only as
// long as ctx allows: a holder that is stopped or stuck must not stall the
// waiter past its own deadline. flock has no timeout, so the wait is
// LOCK_NB polled.
func LockFile(ctx context.Context, f *os.File) error {
	return pollLock(ctx, func() (bool, error) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return err == nil, err
	})
}

// UnlockFile releases LockFile's lock.
func UnlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
