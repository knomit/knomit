//go:build !windows

package knomitapi

import (
	"context"
	"errors"
	"os"
	"syscall"
)

// lockFile takes an exclusive flock, waiting for a holder — a second kb
// waits for the first one's refresh to finish, then finds the new pair on
// disk — but only as long as ctx allows (3a review N5): a holder that is
// stopped or stuck must not stall this kb past its own deadline. flock has
// no timeout, so the wait is LOCK_NB polled.
func lockFile(ctx context.Context, f *os.File) error {
	return pollLock(ctx, func() (bool, error) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return err == nil, err
	})
}

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
