//go:build desktop

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// bundlePathFor returns the .app bundle containing exe, by walking up past
// Contents/MacOS. It errors when exe is not inside a bundle — a dev build run
// straight from `go build` — because there is nothing meaningful to relaunch.
func bundlePathFor(exe string) (string, error) {
	dir := filepath.Dir(exe) // .../Contents/MacOS
	if filepath.Base(dir) != "MacOS" {
		return "", fmt.Errorf("not inside a .app bundle: %s", exe)
	}
	contents := filepath.Dir(dir)
	if filepath.Base(contents) != "Contents" {
		return "", fmt.Errorf("not inside a .app bundle: %s", exe)
	}
	bundle := filepath.Dir(contents)
	if !strings.HasSuffix(bundle, ".app") {
		return "", fmt.Errorf("not inside a .app bundle: %s", exe)
	}
	return bundle, nil
}

// relaunchTarget resolves the .app bundle this executable lives in — the
// pure, side-effect-free half of a restart. NativeService.RestartApp calls
// this BEFORE releaseInstance specifically so a resolution failure (a dev
// build with no bundle, or one moved/deleted since launch) is reported
// without this instance's server having been torn down first.
func relaunchTarget() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return bundlePathFor(exe)
}

// relaunch starts a fresh instance of this app at bundle (as resolved by
// relaunchTarget) and returns once `open` itself has finished — using Run,
// not Start, so a launch failure (bundle moved/deleted/damaged between
// resolution and this call, or Gatekeeper refusing it) is actually observed:
// Start only reports whether `open` was exec'd, not whether it went on to
// launch anything.
//
// The caller (see NativeService.RestartApp) MUST have already released the
// single-instance lockfile before calling this: `open -n` does not wait for
// the new process to finish starting, and singleinstance.Acquire is a
// one-shot check, not a retry loop — if the new process runs it while this
// one still holds the lock, it sees ErrAlreadyRunning and exits immediately,
// silently eating the restart.
func relaunch(bundle string) error {
	return exec.Command("open", "-n", bundle).Run()
}

// revealInFileManager opens the containing directory with the file selected.
func revealInFileManager(path string) error {
	return exec.Command("open", "-R", path).Start()
}
