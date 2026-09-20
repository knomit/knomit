//go:build windows

package userdirs

import (
	"fmt"
	"os"
	"path/filepath"
)

// stateDir is %LOCALAPPDATA% — the per-user, per-machine location Windows
// reserves for application state that is NOT worth roaming.
//
// LocalAppData rather than AppData (roaming) is deliberate and matters for the
// files callers put here. A server lockfile records the port and PID of a
// process running on THIS machine, and an updater's state records what this
// machine's installer is part-way through; both are meaningless on another
// machine, and on a roaming profile they would be copied to one and read back
// as truth.
//
// os.UserConfigDir returns the ROAMING directory, so it is not used here.
func stateDir() (string, error) {
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return local, nil
	}
	// LOCALAPPDATA is set in every normal interactive session; it can be
	// missing under a service account or a stripped environment. Reconstruct
	// the documented default rather than failing outright, which is what
	// os.UserCacheDir does for the same variable.
	home, err := homeDir()
	if err != nil {
		return "", fmt.Errorf("%%LOCALAPPDATA%% is unset and %w", err)
	}
	return filepath.Join(home, "AppData", "Local"), nil
}

// homeDir is the user's profile directory. os.UserHomeDir reads only
// %USERPROFILE% on Windows, so that is the variable the message names.
func homeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(
			"cannot locate your Windows profile: %w; run from a session where "+
				"%%LOCALAPPDATA%% or %%USERPROFILE%% is set", err)
	}
	return home, nil
}
