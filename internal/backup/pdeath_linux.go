//go:build linux

package backup

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal asks the kernel to SIGKILL the agent if knomit dies,
// as a SECOND line of defence on Linux (the deployment target: the Dockerfile
// image is Debian).
//
// The primary mechanism is portable and does not depend on the OS: knomit's
// death closes the write end of the agent's stdin, the agent reads EOF and
// shuts down. That already covers SIGKILL, since the kernel closes the pipe
// when the process dies. Pdeathsig adds cover for the case the pipe cannot:
// an agent wedged inside a syscall that never returns to its read loop.
func setParentDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
