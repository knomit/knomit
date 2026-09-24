//go:build !windows

package knomitapi

import (
	"os"
	"syscall"
)

// lockFile takes a BLOCKING exclusive flock: a second kb waits for the first
// one's refresh to finish, then finds the new pair on disk.
func lockFile(f *os.File) error   { return syscall.Flock(int(f.Fd()), syscall.LOCK_EX) }
func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
