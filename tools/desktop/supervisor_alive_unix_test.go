//go:build desktop && !windows

package main

import "syscall"

// processAlive probes pid with signal 0. A server the test's supervisor
// started is reaped by its Wait; one orphaned by a killed fake desktop is
// reaped by init/launchd, so a gone process does not linger as a zombie here.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
