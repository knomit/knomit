package singleinstance

import (
	"os"
	"testing"
)

// isAlive is unexported, so these live in the internal test package alongside
// it rather than in singleinstance_test.go.

// The live-PID probe, asserted on every OS rather than only where it happened
// to work.
//
// TestAcquire_LivePID_ReturnsErrAlreadyRunning covers the same ground through
// Acquire; this states the property the Windows implementation exists for.
// isAlive must answer TRUE for a process that is demonstrably running — this
// one. The Unix probe (Signal(0)) returns "not supported by windows" for every
// PID, so before the split the shared implementation reported every process
// dead, the single-instance guard never fired, and a second tray could start
// over a running one.
func TestIsAlive_ReportsThisProcessAlive(t *testing.T) {
	if !isAlive(os.Getpid()) {
		t.Errorf("isAlive(%d) = false for the running test process; the single-instance guard can never fire", os.Getpid())
	}
}

// The other half: a PID that cannot be ours must read dead, or a stale
// lockfile would refuse every future start.
//
// 0x7FFFFFFE is far outside any normal allocation range on either platform.
//
// NEGATIVE and ZERO pids are deliberately NOT tested, and must not be added:
// on Unix they are not "invalid", they are BROADCASTS. kill(-1, 0) signals
// every process the caller may signal and returns success, so isAlive(-1)
// answers true on Linux and macOS; kill(0, 0) means the caller's own process
// group and answers true as well. Either row would make this test fail off
// Windows — the precise mistake this whole change set exists to remove.
func TestIsAlive_ReportsAnImpossiblePIDDead(t *testing.T) {
	const impossible = 0x7FFFFFFE
	if isAlive(impossible) {
		t.Errorf("isAlive(%d) = true, want false; a stale lockfile would refuse every future start", impossible)
	}
}
