//go:build !windows

package singleinstance

import (
	"os"
	"syscall"
)

// isAlive reports whether the process is running. On Unix, FindProcess never
// errors, so we send signal 0 to probe.
//
// A process owned by another user answers EPERM, which is reported as dead:
// the lockfile is per-user, so a PID this user cannot signal is a PID that has
// been recycled by somebody else's process, not our tray. See the Windows
// implementation for the same decision made there.
func isAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}
