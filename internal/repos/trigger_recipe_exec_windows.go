//go:build windows

package repos

import (
	"os/exec"
	"sync/atomic"
	"unsafe"

	"github.com/rs/zerolog/log"
	"golang.org/x/sys/windows"
)

// procGroup on Windows is a Job Object [M3]: created with
// JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE before the start; the child is assigned
// right after Start through a handle from OpenProcess(SET_QUOTA|TERMINATE)
// (os.Process does not expose its own); the cancel is TerminateJobObject,
// which kills the child and every process it started in the job.
//
// After a CLEAN exit the kill-on-close limit is CLEARED before the handle is
// closed: closing the last handle of a job with that limit kills everything
// still in it, which would kill a grandchild the recipe detached on purpose
// (status `spawned`) — something Unix never does to a finished exec's group.
// On a timeout or stop the handle is closed with the limit still set, so
// anything left in the job dies with it.
//
// Known gap (Risk 8, not hardened): a child that starts a grandchild before
// it is assigned leaves that grandchild outside the job. os/exec exposes no
// suspended start.
type procGroup struct {
	job      windows.Handle
	assigned atomic.Bool // set by started, read by the cancel (another goroutine)
}

func newProcGroup(cmd *exec.Cmd) *procGroup {
	g := &procGroup{}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		log.Warn().Err(err).Msg("recipe exec: CreateJobObject failed; only the direct child can be killed")
		return g
	}
	if err := setKillOnClose(job, true); err != nil {
		log.Warn().Err(err).Msg("recipe exec: SetInformationJobObject failed; only the direct child can be killed")
		_ = windows.CloseHandle(job)
		return g
	}
	g.job = job
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := windows.TerminateJobObject(job, 1); err == nil && g.assigned.Load() {
			return nil
		}
		return cmd.Process.Kill() // the fallback when the child never made it into the job
	}
	return g
}

// started assigns the child to the job.
func (g *procGroup) started(cmd *exec.Cmd) {
	if g.job == 0 {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		log.Warn().Err(err).Msg("recipe exec: OpenProcess failed; only the direct child can be killed")
		return
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(g.job, h); err != nil {
		log.Warn().Err(err).Msg("recipe exec: AssignProcessToJobObject failed; only the direct child can be killed")
	} else {
		g.assigned.Store(true)
	}
}

// close releases the job; after a clean exit the kill-on-close limit is
// cleared first so a detached grandchild survives.
func (g *procGroup) close(clean bool) {
	if g.job == 0 {
		return
	}
	if clean {
		if err := setKillOnClose(g.job, false); err != nil {
			log.Warn().Err(err).Msg("recipe exec: clearing kill-on-close failed; a detached grandchild may be killed")
		}
	}
	_ = windows.CloseHandle(g.job)
	g.job = 0
}

func setKillOnClose(job windows.Handle, on bool) error {
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	if on {
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	}
	_, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)))
	return err
}
