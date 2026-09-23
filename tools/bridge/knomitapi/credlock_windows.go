//go:build windows

package knomitapi

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFile takes a BLOCKING exclusive lock on the file's first byte: a second
// kb waits for the first one's refresh to finish, then finds the new pair on
// disk.
func lockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &ol)
}

func unlockFile(f *os.File) error {
	var ol windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &ol)
}
