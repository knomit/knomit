//go:build desktop && linux

package main

import (
	"os/exec"
	"runtime"
	"syscall"
)

// configureChild asks the kernel to SIGTERM the server when its parent dies:
// the backstop behind stdin EOF, for a desktop that is SIGKILLed while the
// server is not reading (a model download, say).
func configureChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGTERM
}

// startChild starts cmd from a goroutine that holds its OS thread locked for
// the child's whole life. Pdeathsig fires when the THREAD that forked the
// child exits, not the process (golang/go#27505): an ordinary goroutine's
// thread can be retired by the runtime at any time, which would SIGTERM a
// healthy server. The locked goroutine therefore also does the Wait, and only
// unlocks once the child is gone.
func startChild(cmd *exec.Cmd) (childProc, error) {
	started := make(chan error, 1)
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := cmd.Start(); err != nil {
			started <- err
			return
		}
		started <- nil
		done <- cmd.Wait()
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return &lockedChild{cmd: cmd, done: done}, nil
}

type lockedChild struct {
	cmd  *exec.Cmd
	done chan error
}

func (c *lockedChild) wait() error { return <-c.done }
func (c *lockedChild) kill() error { return c.cmd.Process.Kill() }
