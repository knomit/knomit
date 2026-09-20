//go:build windows

package apppaths

import (
	"fmt"
	"os"
	"path/filepath"
)

// stateDir is %LOCALAPPDATA%\knomit — the per-user, per-machine location
// Windows reserves for application state that is NOT worth roaming.
//
// LocalAppData rather than AppData (roaming) is deliberate and matters for
// the files that live here. server.json records the port and PID of the
// server running on THIS machine, and update.json records what this machine's
// installer is part-way through; both are meaningless on another machine, and
// on a roaming profile they would be copied to one and read back as truth.
//
// os.UserConfigDir returns the roaming directory, so it is not used here.
func stateDir() (string, error) {
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, appDir), nil
	}
	// LOCALAPPDATA is set in every normal interactive session; it can be
	// missing under a service account or a stripped environment. Reconstruct
	// the documented default rather than failing outright, which is what
	// os.UserCacheDir does for the same variable.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(
			"cannot locate your Windows profile: %%LOCALAPPDATA%% is unset and %w. "+
				"Set KNOMIT_HOME to the directory knomit should use, or run from a session "+
				"where %%LOCALAPPDATA%% or %%USERPROFILE%% is set", err)
	}
	return filepath.Join(home, "AppData", "Local", appDir), nil
}

// defaultHome is %LOCALAPPDATA%\knomit\home.
//
// Windows has never shipped, so there is no ~/.knomit installed base to
// migrate and no second resolution branch to carry: nothing on Windows
// consults %USERPROFILE%\.knomit.
func defaultHome() (string, error) {
	state, err := stateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, homeSubdir), nil
}
