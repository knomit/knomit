//go:build desktop

package main

// appImageEnv is the variable the AppImage runtime exports before handing over
// to AppRun: the absolute path of the .AppImage FILE the user launched.
const appImageEnv = "APPIMAGE"

// executableRelaunchTarget resolves what to re-exec on the non-darwin builds
// (darwin relaunches the .app bundle instead — see restart_darwin.go).
//
// os.Executable() is the obvious answer and the wrong one on the artifact we
// actually ship for Linux: inside an AppImage it points at
// /tmp/.mount_Knomitxxxxxx/usr/bin/knomit-desktop, a path on a squashfs the
// runtime unmounts the moment this process exits. Re-execing it hands the
// replacement a binary that is about to disappear, and one that never sees
// AppRun's environment setup even if it wins the race. The failure is
// success-shaped: exec.Start() reports only that the fork succeeded, so
// RestartApp returns nil, quits this instance, and the app simply vanishes.
//
// $APPIMAGE is the path that outlives us, so it wins whenever the runtime set
// it. Outside an AppImage (a plain binary, a dev build) it is unset and
// os.Executable is exactly right.
//
// Lives in a platform-neutral file, and takes its two dependencies as
// parameters, so the selection is unit-testable on every platform rather than
// only on the one that ships it.
func executableRelaunchTarget(getenv func(string) string, executable func() (string, error)) (string, error) {
	if p := getenv(appImageEnv); p != "" {
		return p, nil
	}
	return executable()
}
