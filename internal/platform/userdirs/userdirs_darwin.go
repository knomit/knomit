//go:build darwin

package userdirs

import "path/filepath"

// stateDir is ~/Library/Application Support, where macOS expects per-user
// application state that is neither a cache nor a document.
func stateDir() (string, error) {
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support"), nil
}
