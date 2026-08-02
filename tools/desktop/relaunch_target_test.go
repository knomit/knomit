//go:build desktop

package main

import (
	"errors"
	"testing"
)

// The Linux artifact we ship is an AppImage, and inside one os.Executable()
// resolves into the runtime's /tmp/.mount_* squashfs — a path that is unmounted
// the instant this process exits. Re-execing it hands the replacement a binary
// that is about to vanish, and the failure is success-shaped: exec.Start()
// reports the fork worked, RestartApp returns nil, this instance quits, and the
// app is simply gone. $APPIMAGE is the path that outlives us.
func TestRelaunchTargetPrefersTheAppImage(t *testing.T) {
	getenv := func(k string) string {
		if k == appImageEnv {
			return "/home/u/Apps/Knomit-0.5.1-linux-amd64.AppImage"
		}
		return ""
	}
	executable := func() (string, error) {
		return "/tmp/.mount_Knomit4Xk2p/usr/bin/knomit-desktop", nil
	}

	got, err := executableRelaunchTarget(getenv, executable)
	if err != nil {
		t.Fatalf("executableRelaunchTarget: %v", err)
	}
	if got != "/home/u/Apps/Knomit-0.5.1-linux-amd64.AppImage" {
		t.Errorf("relaunch target = %q, want the .AppImage file; a squashfs path is torn down when we exit", got)
	}
}

// Outside an AppImage the runtime sets nothing, and the executable itself is
// exactly the right thing to start again.
func TestRelaunchTargetFallsBackToTheExecutable(t *testing.T) {
	got, err := executableRelaunchTarget(func(string) string { return "" },
		func() (string, error) { return "/usr/local/bin/knomit-desktop", nil })
	if err != nil {
		t.Fatalf("executableRelaunchTarget: %v", err)
	}
	if got != "/usr/local/bin/knomit-desktop" {
		t.Errorf("relaunch target = %q, want the executable path", got)
	}
}

// RestartApp resolves the target BEFORE it tears this instance's server down,
// precisely so a resolution failure is reported with nothing yet broken. That
// only holds if the failure is actually propagated.
func TestRelaunchTargetPropagatesTheExecutableError(t *testing.T) {
	want := errors.New("no executable")
	if _, err := executableRelaunchTarget(func(string) string { return "" },
		func() (string, error) { return "", want }); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v", err, want)
	}
}
