//go:build desktop && !darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// relaunchTarget resolves the executable to re-exec. Split out from relaunch
// for symmetry with the darwin build — see its relaunchTarget comment for why
// NativeService.RestartApp calls this before releaseInstance — even though
// os.Executable rarely fails in practice.
func relaunchTarget() (string, error) {
	return os.Executable()
}

// relaunch re-execs exe (as resolved by relaunchTarget). On Linux the desktop
// ships as an AppImage or a plain binary; either way the executable path is
// the thing to start again. Start (not Run) is fine here, unlike on darwin:
// this execs the target directly, so a failure to start it is reported by
// Start itself — there is no indirection through a launcher process whose own
// exit code says nothing about whether the app came up.
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
