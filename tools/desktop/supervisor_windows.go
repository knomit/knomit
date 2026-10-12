//go:build desktop && windows

package main

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"
)

// configureChild keeps knomit.exe (a console binary) from opening a console
// window under the GUI desktop.
func configureChild(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NO_WINDOW
}

// startChild starts cmd and puts it in a Job Object with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, the pattern of
// internal/repos/trigger_recipe_exec_windows.go (kb gotcha
// repos/triggers/recipes/process-groups, 116de6c4). The desktop holds the only
// handle; when the desktop dies, however it dies, the kernel closes it and
// kills the server and everything in its job. The server's own recipe children
// sit in nested jobs of their own, inside this one.
//
// Unlike the recipe case, the limit is NOT cleared on a clean exit: nothing
// the server starts is meant to outlive it.
//
// Known gap, as there: a process the server starts before it is assigned
// (microseconds after Start) is outside the job. os/exec has no suspended
// start. The server starts nothing that early.
func startChild(cmd *exec.Cmd) (childProc, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		log.Warn().Err(err).Msg("server job: CreateJobObject failed; the server outlives a killed desktop until it reads stdin EOF")
		job = 0
	} else if err := setKillOnJobClose(job); err != nil {
		log.Warn().Err(err).Msg("server job: SetInformationJobObject failed; the server outlives a killed desktop until it reads stdin EOF")
		_ = windows.CloseHandle(job)
		job = 0
	}
	if err := cmd.Start(); err != nil {
		if job != 0 {
			_ = windows.CloseHandle(job)
		}
		return nil, err
	}
	if job != 0 {
		h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
		if err != nil {
			log.Warn().Err(err).Msg("server job: OpenProcess failed; the server is not in the job")
		} else {
			if err := windows.AssignProcessToJobObject(job, h); err != nil {
				log.Warn().Err(err).Msg("server job: AssignProcessToJobObject failed; the server is not in the job")
			}
			_ = windows.CloseHandle(h)
		}
	}
	return &jobChild{cmd: cmd, job: job}, nil
}

type jobChild struct {
	cmd *exec.Cmd
	mu  sync.Mutex // guards job: wait closes it while kill may use it
	job windows.Handle
}

// wait returns once the server has exited, then closes the job (killing
// anything the server left in it).
func (c *jobChild) wait() error {
	err := c.cmd.Wait()
	c.mu.Lock()
	if c.job != 0 {
		_ = windows.CloseHandle(c.job)
		c.job = 0
	}
	c.mu.Unlock()
	return err
}

func (c *jobChild) kill() error {
	c.mu.Lock()
	job := c.job
	var err error = windows.ERROR_INVALID_HANDLE
	if job != 0 {
		err = windows.TerminateJobObject(job, 1)
	}
	c.mu.Unlock()
	if err == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}

func setKillOnJobClose(job windows.Handle) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return err
}
