//go:build windows

package pki

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// LockFile takes an exclusive lock on f's first byte, waiting for a holder
// but only as long as ctx allows. LockFileEx with LOCKFILE_FAIL_IMMEDIATELY,
// polled: the blocking form cannot be abandoned.
func LockFile(ctx context.Context, f *os.File) error {
	return pollLock(ctx, func() (bool, error) {
		var ol windows.Overlapped
		err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return false, nil
		}
		return err == nil, err
	})
}

// UnlockFile releases LockFile's lock.
func UnlockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
