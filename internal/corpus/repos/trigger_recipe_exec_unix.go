//go:build !windows

package repos

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// procGroup on Unix is the child's own process group: Setpgid at fork, so
// the child and everything it starts share a pgid equal to its pid, and the
// cancel kills the GROUP (kill(-pgid, SIGKILL)), grandchildren included.
// Measured (proposal): a grandchild holding stdout made a plain
// CommandContext's Wait hang 30 s; group kill + WaitDelay returned in 301 ms.
// A finished exec's group is never signalled, so a grandchild the recipe
// detached on purpose outlives the call (status `spawned`).
type procGroup struct{}

func newProcGroup(cmd *exec.Cmd) *procGroup {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	return &procGroup{}
}

// started: nothing to do; the group exists from the fork.
func (g *procGroup) started(*exec.Cmd) {}

// close: nothing to release.
func (g *procGroup) close(clean bool) {}
