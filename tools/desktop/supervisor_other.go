//go:build desktop && !linux && !windows

package main

import "os/exec"

// configureChild has nothing to add on macOS: there is no parent-death signal,
// and stdin EOF (the desktop's pipe closing when it exits, however it exits)
// is the standard answer. kqueue NOTE_EXIT on the parent's pid would be the
// alternative; the server does not need it because it already watches stdin.
func configureChild(*exec.Cmd) {}

// startChild starts cmd on the calling goroutine.
func startChild(cmd *exec.Cmd) (childProc, error) {
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return plainChild{cmd}, nil
}

type plainChild struct{ cmd *exec.Cmd }

func (c plainChild) wait() error { return c.cmd.Wait() }
func (c plainChild) kill() error { return c.cmd.Process.Kill() }
