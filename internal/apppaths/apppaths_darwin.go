//go:build darwin

package apppaths

import (
	"os"
	"path/filepath"
)

// stateDir is ~/Library/Application Support/knomit, where macOS expects
// per-user application state that is not a cache and not a document.
func stateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", appDir), nil
}
