//go:build linux

package apppaths

import (
	"os"
	"path/filepath"
)

// stateDir honours $XDG_STATE_HOME and falls back to the XDG-documented
// default of ~/.local/state.
func stateDir() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, appDir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", appDir), nil
}
