//go:build !windows

package auth

import (
	"errors"
	"os"
	"syscall"
)

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
