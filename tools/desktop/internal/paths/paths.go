// Package paths resolves platform-appropriate filesystem locations for the
// desktop app's lockfile, logs, and autostart artifacts.
package paths

import (
	"os"
	"path/filepath"
)

// StateDir returns the directory where the desktop app persists state
// (lockfile, etc.). The directory is NOT created by this function.
func StateDir() (string, error) { return stateDir() }

// DownloadsDir returns the user's Downloads folder, where the Export save dialog
// opens: ~/Downloads on macOS, the Downloads known folder on Windows, and on Linux
// XDG_DOWNLOAD_DIR (from the environment, then ~/.config/user-dirs.dirs) or
// ~/Downloads. It is a SUGGESTION for a dialog, never a place the app writes to
// on its own; "" when the home directory cannot be found.
func DownloadsDir() string { return downloadsDir() }

// homeDownloads is ~/Downloads, "" without a home directory.
func homeDownloads() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "Downloads")
}

// LogsDir returns the directory where the desktop app writes log files.
// The directory is NOT created by this function.
func LogsDir() (string, error) { return logsDir() }

// LockfilePath returns the absolute path to the server lockfile
// (<StateDir>/server.json).
func LockfilePath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "server.json"), nil
}

// UpdateStatePath returns the absolute path to the updater's cross-launch
// state (<StateDir>/update.json).
func UpdateStatePath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "update.json"), nil
}
