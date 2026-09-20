//go:build darwin

package paths

import (
	"os"
	"path/filepath"

	"knomit/internal/config"
)

func stateDir() (string, error) { return config.StateDir() }

// logsDir stays here rather than in config: ~/Library/Logs is a macOS
// convention for logs specifically, and nothing outside the desktop app wants
// it. config owns only what more than one binary has to agree on.
func logsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "knomit"), nil
}
