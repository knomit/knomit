//go:build !windows

package apppaths

import (
	"fmt"
	"os"
	"path/filepath"
)

// defaultHome is ~/.knomit on every non-Windows platform.
//
// Unchanged, and deliberately so: the Windows default moved under
// %LOCALAPPDATA% because Windows support had not shipped and there was no
// installed base. macOS and Linux both have one. Moving them to
// ~/Library/Application Support or $XDG_DATA_HOME is a separate decision with
// a migration attached, and this is not it.
//
// The error is returned rather than swallowed for the same reason as on
// Windows: a home that cannot be resolved must stop startup with something the
// operator can act on, not resolve to "/.knomit".
func defaultHome() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf(
			"cannot locate your home directory: %w. Set KNOMIT_HOME to the directory "+
				"knomit should use, or run from a session where $HOME is set", err)
	}
	return filepath.Join(home, "."+appDir), nil
}
