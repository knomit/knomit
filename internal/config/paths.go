// Per-user, per-OS locations knomit installs itself into: the DATA ROOT and the
// STATE DIRECTORY, spelled once, here.
//
// These live in config because config already owns Config.Home — the resolved
// data root — and "where does it go when the operator has not said" is the same
// question one step earlier. A separate package for the two constants below
// would be a package for two strings.
//
// # The split
//
// WHERE an OS keeps a user's directories is OS knowledge and lives in
// internal/platform/userdirs: the Windows %LOCALAPPDATA% reconstruction, the
// XDG lookup, and the rule that an unresolvable directory is an error rather
// than a guess. userdirs returns BASE directories and knows no application.
// What knomit calls its folder is the application knowledge, and it is the two
// constants below — joined on here, so there is exactly one spelling of the
// name however many binaries ask.
//
// THREE BINARIES HAVE TO AGREE, which is why this is not left to each caller.
// `knomit serve` resolves the data root through Load, `kb` resolves the
// lockfile to find a running server, and knomit-desktop resolves both. When
// each worked it out for itself the answers drifted: tools/bridge had no
// Windows case at all and silently fell back to a default port, while the
// desktop was already writing server.json under %LOCALAPPDATA%. A user whose
// data root depends on which binary started first re-downloads 617MB of models
// and gets a second identity, so the agreement is load-bearing rather than tidy.
//
// Not every appearance of the word in a path is here — tools/desktop's
// paths_darwin spells ~/Library/Logs/knomit, and tools/bridge/antigravity has
// its own plugin directory name. Those are different locations that nothing
// else has to agree with. These two are the ones the binaries must resolve
// identically.
//
// KNOMIT_HOME overrides the data root and is layered in Load, not here: these
// answer "where does knomit go by default", and Load answers "where has the
// operator put it".

package config

import (
	"fmt"
	"path/filepath"

	"knomit/internal/platform/userdirs"
)

// appDir is the directory knomit owns inside whatever per-user location the
// OS designates. One spelling, one place.
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
func StateDir() (string, error) {
	base, err := userdirs.StateDir()
	if err != nil {
		return "", withHomeHint(err)
	}
	return filepath.Join(base, appDir), nil
}

// DefaultHome is the default knomit data root — control.db, repos/, models/,
// the SSH keypair, bin/ and knomit.toml. It is what Config.Home falls back to
// when KNOMIT_HOME and KNOMIT_REPO are both unset. The directory is NOT
// created here.
//
// An unresolvable home is an error and an empty string, never a guess.
// internal/platform/userdirs owns that rule and the reason for it.
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

// withHomeHint adds knomit's own way out to an OS-level resolution failure.
//
// It is here rather than in userdirs because KNOMIT_HOME is application
// knowledge: userdirs can say "%LOCALAPPDATA% is unset", and only this package
// knows there is an override that makes that survivable. Tests assert the
// variable is named, because an error that stops startup without saying what to
// do about it is the failure being fixed, not a fix.
func withHomeHint(err error) error {
	return fmt.Errorf("%w. Set KNOMIT_HOME to the directory knomit should use", err)
}
