//go:build !windows

package repos

import (
	"errors"
	"syscall"
)

// processAlive reports whether pid names a live process. A zombie awaiting
// its reaper still answers signal 0; the tests poll, so the reaper (init or
// launchd, once the helper that started it is dead) gets its moment.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// killProcess ends a leftover test process.
func killProcess(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }
