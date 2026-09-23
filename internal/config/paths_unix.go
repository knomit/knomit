//go:build !windows

package config

import (
	"path/filepath"

	"knomit/internal/platform/userdirs"
)

// defaultHome is ~/.knomit on every non-Windows platform.
//
// Unchanged, and deliberately so: the Windows default moved under
// %LOCALAPPDATA% because Windows support had not shipped and there was no
// installed base. macOS and Linux both have one. Moving them to
// ~/Library/Application Support or $XDG_DATA_HOME is a separate decision with
// a migration attached, and this is not it.
//
// Note it hangs off the HOME directory, not off userdirs.StateDir() as Windows
// does — which is exactly the policy this file exists to hold.
func defaultHome() (string, error) {
	home, err := userdirs.HomeDir()
	if err != nil {
		return "", withHomeHint(err)
	}
	return filepath.Join(home, "."+appDir), nil
}

// socketFile is the unix socket's name inside the data root. One spelling,
// for the same reason appDir has one: `knomit serve` opens it and `kb` dials
// it, and a disagreement means the bridge silently falls back to TCP.
const socketFile = "knomit.sock"

// localListenerName is the path of the local authenticated listener for a
// given data root. On unix that is a socket file inside the root, so the
// 0700 root above it is what guards it.
//
// The Windows half cannot do this, because a named pipe does not live in the
// filesystem — hence one function per platform rather than a filepath.Join at
// each call site.
func localListenerName(home string) string {
	return filepath.Join(home, socketFile)
}
