//go:build windows

package config

import "path/filepath"

// defaultHome is %LOCALAPPDATA%\knomit\home — a level BELOW the state
// directory, so the data root does not sit beside server.json and the logs in
// the folder the desktop's "reveal log" menu item opens.
//
// Windows has never shipped, so there is no ~/.knomit installed base to
// migrate and no second resolution branch to carry: nothing on Windows
// consults %USERPROFILE%\.knomit.
//
// Which OS directory this hangs off is POLICY, and policy is why this file
// still exists after the OS logic moved to internal/platform/userdirs.
func defaultHome() (string, error) {
	state, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(state, homeSubdir), nil
}
