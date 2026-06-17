// Package paths resolves platform-appropriate filesystem locations for the
// desktop app's lockfile, logs, and autostart artifacts.
package paths

import "path/filepath"

// StateDir returns the directory where the desktop app persists state
// (lockfile, etc.). The directory is NOT created by this function.
func StateDir() (string, error) { return stateDir() }

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
