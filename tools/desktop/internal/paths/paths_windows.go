//go:build windows

package paths

import (
	"os"
	"path/filepath"
)

// stateDir is %LOCALAPPDATA%\knomit — the per-user, per-machine location
// Windows reserves for application state that is NOT worth roaming.
//
// LocalAppData rather than AppData (roaming) is deliberate and matters for
// the two files that live here. server.json records the port and PID of the
// server running on THIS machine, and update.json records what this machine's
// installer is part-way through; both are meaningless on another machine, and
// on a roaming profile they would be copied to one and read back as truth.
//
// os.UserConfigDir returns the roaming directory, so it is not used here.
func stateDir() (string, error) {
	if local := os.Getenv("LOCALAPPDATA"); local != "" {
		return filepath.Join(local, "knomit"), nil
	}
	// LOCALAPPDATA is set in every normal interactive session; it can be
	// missing under a service account or a stripped environment. Reconstruct
	// the documented default rather than failing outright, which is what
	// os.UserCacheDir does for the same variable.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "AppData", "Local", "knomit"), nil
}

// logsDir is the state directory, as on Linux. Windows has no per-user
// equivalent of macOS's ~/Library/Logs, and a separate subdirectory would mean
// two places to look when a user is asked for their log — the reveal-log menu
// item opens one folder.
func logsDir() (string, error) {
	return stateDir()
}
