//go:build linux

package paths

import (
	"os"
	"path/filepath"
)

func stateDir() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "knomit"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "knomit"), nil
}

func logsDir() (string, error) {
	return stateDir()
}
