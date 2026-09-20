// Package apppaths owns the per-user, per-OS locations knomit installs itself
// into, and it is the ONLY place the "knomit" directory name is spelled.
//
// It exists because three binaries have to agree. `knomit serve` resolves the
// data root through internal/config, `kb` resolves the lockfile to find a
// running server, and knomit-desktop resolves both. When each of them worked
// it out for itself the answers drifted: tools/bridge had no Windows case at
// all and silently fell back to a default port, while the desktop was already
// writing server.json under %LOCALAPPDATA%. A user whose data root depends on
// which binary started first re-downloads 617MB of models and gets a second
// identity, so the agreement is load-bearing rather than tidy.
//
// It is NOT under internal/platform. That tier is pinned by
// TestPlatformKnowsNothingAboutKnomit to know "the OS and the binary, and
// nothing about this application", and the string "knomit" is exactly the
// application knowledge it excludes — a helper there would have to take the
// app name as a parameter, which would put the literal back at the call sites
// and defeat the point of the package.
//
// KNOMIT_HOME overrides the data root and is layered in internal/config, not
// here: this package answers "where does knomit go by default", and config
// answers "where has the operator put it".
package apppaths

import "path/filepath"

// appDir is the directory knomit owns inside whatever per-user location the
// OS designates. One spelling, one package.
const appDir = "knomit"

// homeSubdir is the data root's name inside StateDir on Windows.
//
// Windows has no separate per-user data directory — %LOCALAPPDATA% is both
// "state this machine rebuilt" and "data that is the product" — so the two are
// separated by a level instead of by a variable. Without it, control.db,
// repos/, models/, the keypair and bin/ would sit beside server.json and the
// logs, in the folder the desktop's "reveal log" menu item opens.
const homeSubdir = "home"

// StateDir is where a knomit binary keeps state that describes THIS machine:
// the server lockfile, logs, and the updater's cross-launch state. It is
// per-user and deliberately non-roaming. The directory is NOT created here.
func StateDir() (string, error) { return stateDir() }

// DefaultHome is the default knomit data root — control.db, repos/, models/,
// the SSH keypair, bin/ and knomit.toml. It is what Config.Home falls back to
// when KNOMIT_HOME and KNOMIT_REPO are both unset. The directory is NOT
// created here.
//
// An unresolvable home is an error, never a guess. The bug this package was
// extracted for was `home, _ := os.UserHomeDir()` followed by string
// concatenation: on Windows the discarded error does not degrade to "no home",
// it degrades to "/.knomit", which the OS resolves against the current drive
// as C:\.knomit — writable, plausible, and wrong.
func DefaultHome() (string, error) { return defaultHome() }

// LockfilePath is <StateDir>/server.json: the port and PID of the server
// running on this machine. Shared by the desktop (which writes it) and the
// bridge (which reads it) so the two cannot look in different places.
func LockfilePath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "server.json"), nil
}
