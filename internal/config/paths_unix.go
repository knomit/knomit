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
