//go:build desktop && !darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// relaunchTarget resolves the executable to re-exec — the .AppImage file when
// running from one, this executable otherwise. See executableRelaunchTarget for
// why the AppImage case is not just os.Executable(). Split out from relaunch
// for symmetry with the darwin build — see its relaunchTarget comment for why
// NativeService.RestartApp calls this before releaseInstance.
func relaunchTarget() (string, error) {
	return executableRelaunchTarget(os.Getenv, os.Executable)
}

// relaunch re-execs the target resolved by relaunchTarget: the .AppImage file
// on the Linux artifact we ship, the executable itself on a plain binary.
// Either way it is a single file that starts the whole app. Start (not Run) is
// fine here, unlike on darwin: this execs the target directly, so a failure to
// start it is reported by Start itself — there is no indirection through a
// launcher process whose own exit code says nothing about whether the app came
// up. That does mean Start's success says nothing about the target being the
// RIGHT one, which is precisely why relaunchTarget must not hand it a path
// inside a squashfs mount that is about to go away.
//
// As on darwin, the caller (NativeService.RestartApp) MUST release the
// single-instance lockfile before calling this — see the comment on the
// darwin relaunch for why the naive "spawn then quit" order races.
func relaunch(exe string) error {
	return exec.Command(exe).Start()
}

// revealInFileManager opens the log file's directory with the desktop's default
// handler. There is no portable "select this file", so the directory is opened.
func revealInFileManager(path string) error {
	return exec.Command("xdg-open", filepath.Dir(path)).Start()
}
