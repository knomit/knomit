//go:build unix

package homelock

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes a non-blocking exclusive flock, reporting (false, nil) when
// another descriptor already holds it.
//
// flock(2) associates the lock with the open file DESCRIPTION rather than the
// process, so two independent opens of the same path conflict even inside one
// process. That is what makes this testable without spawning children, and it
// is also why Acquire must not be called twice for the same home in one process
// expecting the second to succeed.
func tryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		// EAGAIN on Linux, EWOULDBLOCK on BSD/macOS; they are the same value.
		return false, nil
	default:
		return false, err
	}
}

func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
