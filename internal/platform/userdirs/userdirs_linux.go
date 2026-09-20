//go:build linux

package userdirs

import (
	"os"
	"path/filepath"
)

// stateDir honours $XDG_STATE_HOME and falls back to the XDG-documented
// default of ~/.local/state.
//
// An empty XDG_STATE_HOME counts as unset, per the XDG spec's own wording, and
// not doing so is a live bug class rather than pedantry: joining onto "" yields
// a RELATIVE path that resolves against the working directory.
func stateDir() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return xdg, nil
	}
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state"), nil
}
