//go:build desktop && windows

package main

import "testing"

// Windows backstop: a server that IGNORES stdin EOF still ends when its
// desktop is killed, because the desktop held the only handle to a Job Object
// with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE and the kernel closed it.
func TestSupervisor_KilledDesktopEndsTheServerThroughTheJob(t *testing.T) {
	killParentAndExpectChildGone(t, "ignore-stdin")
}
