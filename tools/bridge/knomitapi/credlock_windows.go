//go:build windows

package knomitapi

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes an exclusive lock on the file's first byte, waiting for a
// holder — a second kb waits for the first one's refresh to finish, then
// finds the new pair on disk — but only as long as ctx allows (3a review
// N5). LockFileEx with LOCKFILE_FAIL_IMMEDIATELY, polled: the blocking form
// cannot be abandoned.
func lockFile(ctx context.Context, f *os.File) error {
	return pollLock(ctx, func() (bool, error) {
		var ol windows.Overlapped
		err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &ol)
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return false, nil
		}
		return err == nil, err
	})
}

func unlockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
