//go:build desktop && linux

package main

import "testing"

// Linux backstop: a server that IGNORES stdin EOF still ends when its desktop
// is SIGKILLed, because startChild set Pdeathsig = SIGTERM from a goroutine
// holding its thread for the child's life. Without Pdeathsig (or with the
// thread released early) the server would outlive the desktop.
func TestSupervisor_KilledDesktopEndsTheServerThroughPdeathsig(t *testing.T) {
	killParentAndExpectChildGone(t, "ignore-stdin")
}
