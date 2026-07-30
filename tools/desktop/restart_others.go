//go:build desktop && !darwin

package main

import (
	"os"
	"os/exec"
	"path/filepath"
)

// relaunch re-execs the binary. On Linux the desktop ships as an AppImage or a
// plain binary; either way the executable path is the thing to start again.
//
// As on darwin, the caller (NativeService.RestartApp) MUST release the
// single-instance lockfile before calling this — see the comment on the
// darwin relaunch for why the naive "spawn then quit" order races.
func relaunch() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return exec.Command(exe).Start()
}

// revealInFileManager opens the log file's directory with the desktop's default
// handler. There is no portable "select this file", so the directory is opened.
func revealInFileManager(path string) error {
	return exec.Command("xdg-open", filepath.Dir(path)).Start()
}
