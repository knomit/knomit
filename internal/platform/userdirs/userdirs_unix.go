//go:build !windows

package userdirs

import (
	"fmt"
	"os"
)

// homeDir is the user's home directory. os.UserHomeDir consults $HOME on unix,
// so that is the variable the message names.
func homeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate your home directory: %w; run from a session where $HOME is set", err)
	}
	return home, nil
}
