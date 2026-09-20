// Package apppaths owns the per-user, per-OS locations knomit installs itself
// into: it is the only place the DATA ROOT and the STATE DIRECTORY are spelled.
//
// Not the only place the word appears in a path — tools/desktop's paths_darwin
// spells ~/Library/Logs/knomit, and tools/bridge/antigravity has its own plugin
// directory name. Those are different locations that nothing else has to agree
// with. The two this package owns are the ones three binaries must resolve
// identically.
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
// It is NOT under internal/platform, and to be precise about why: the archtest
// would NOT have caught it. TestPlatformKnowsNothingAboutKnomit only forbids
// IMPORTS of knomit packages outside the tier, and this package imports nothing
// but the standard library, so it would have passed there.
//
// The placement rests on the tier's documented property instead — internal/
// platform "knows the OS and the binary; nothing about this application" — and
// the constant below is exactly that application knowledge. The tempting
// workaround, taking the app name as a parameter the way logging.Options takes
// its config values, is what makes this the wrong tier rather than a fixable
// fit: logging.Options parameterises a value the caller already owns, whereas
// parameterising the app name would put the literal back at every call site and
// give the three binaries three chances to disagree, which is the one thing
// this package exists to prevent.
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
