//go:build windows

package paths

import "knomit/internal/apppaths"

// stateDir is %LOCALAPPDATA%\knomit, resolved by internal/apppaths.
//
// It is NOT spelled here. tools/bridge needs the same directory to find
// server.json, and it cannot import this package — tools/desktop/internal/ is
// importable only from under tools/desktop/ — so a copy lived in the bridge
// and drifted: it had no Windows case at all and fell back to a default port
// while the desktop wrote the lockfile here. One owner, no second spelling.
func stateDir() (string, error) { return apppaths.StateDir() }

// logsDir is the state directory, as on Linux. Windows has no per-user
// equivalent of macOS's ~/Library/Logs, and a separate subdirectory would mean
// two places to look when a user is asked for their log — the reveal-log menu
// item opens one folder.
func logsDir() (string, error) { return stateDir() }
