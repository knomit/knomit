//go:build windows

package singleinstance

import "golang.org/x/sys/windows"

// isAlive reports whether the process is running.
//
// The Unix probe (Signal(0)) cannot be used here: Windows has no signals, and
// os.Process.Signal returns "not supported by windows" for every PID, live or
// dead. That made isAlive answer false unconditionally, so the single-instance
// guard never fired and a second tray could start over a running one
// (TestAcquire_LivePID_ReturnsErrAlreadyRunning).
//
// Liveness is decided by waiting on the process handle with a zero timeout: a
// process object is signalled when the process exits, so WAIT_TIMEOUT means
// still running and WAIT_OBJECT_0 means exited. GetExitCodeProcess is the more
// obvious call and is wrong — a process that exits with status 259 is
// indistinguishable from STILL_ACTIVE.
//
// Waiting needs SYNCHRONIZE; PROCESS_QUERY_LIMITED_INFORMATION is the minimum
// right that works across privilege levels for the open itself.
//
// Any failure to open the handle counts as dead, which matches what the Unix
// side does with EPERM. The two cases are a PID that no longer exists and a
// PID recycled by a process this user may not touch; neither is our tray, and
// for a single-instance guard a false "dead" costs a redundant start (the
// lockfile is immediately overwritten) while a false "alive" would refuse to
// start at all.
//
// Note the asymmetry with Unix: PROCESS_QUERY_LIMITED_INFORMATION is granted
// ACROSS user accounts, so a recycled PID now owned by ANOTHER user opens
// successfully and reads ALIVE here, where the Unix probe would get EPERM and
// say dead. The consequence is bounded and self-correcting — the guard
// declines to start once, and the user's next attempt is after the lockfile
// has been rewritten — but it is the reason this must not be "fixed" by
// treating an access error as alive.
func isAlive(pid int) bool {
	const access = windows.SYNCHRONIZE | windows.PROCESS_QUERY_LIMITED_INFORMATION
	h, err := windows.OpenProcess(access, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)

	ev, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	return ev == uint32(windows.WAIT_TIMEOUT)
}
