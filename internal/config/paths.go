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
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

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

// ResolveHome is the data root the operator actually gets: KNOMIT_HOME, else
// the KNOMIT_REPO alias, else DefaultHome. Load layers TOML and the rest on
// top, but WHICH directory is the root is decided here and only here.
func ResolveHome() (string, error) {
	if v := os.Getenv("KNOMIT_HOME"); v != "" {
		return v, nil
	}
	if v := os.Getenv("KNOMIT_REPO"); v != "" {
		return v, nil
	}
	return DefaultHome()
}

// homeAndConfig is the data root the operator gets, tilde-expanded, and the
// knomit.toml found for it ("" when there is none). Load and SocketPath both
// start here, so "resolve, expand, then search" happens in one order in one
// place: searching before expanding looks in a literal "~/..." directory and
// silently skips the operator's knomit.toml.
func homeAndConfig() (home, configPath string, err error) {
	home, err = ResolveHome()
	if err != nil {
		return "", "", fmt.Errorf("config: cannot determine the knomit data root: %w", err)
	}
	if err := expandTilde(&home); err != nil {
		return "", "", fmt.Errorf("config: %w", err)
	}
	return home, findConfigFile(home), nil
}

// SocketPath is the LOCAL AUTHENTICATED LISTENER for the resolved data root:
// <data root>/knomit.sock on unix, and a named pipe called knomit-<hash of
// the data root> in the machine's pipe namespace on Windows (see
// paths_windows.go, which is where the shape of that name is decided) —
// unless the operator named one. It is the local bridge's credential either
// way — the OS tells the server who is dialling, so nothing has to be stored
// or presented.
//
// It resolves the SAME three layers Load does, from the same starting point
// (homeAndConfig) and through the same function (socketFor): KNOMIT_SOCKET,
// else the `socket` key of the knomit.toml Load reads, else the default under
// the tilde-expanded data root. That is the point of it being here rather than
// in the bridge. This file's header records what happened last time each
// binary worked a path out for itself: the answers drifted. A bridge that
// computed a different socket path from the server would not fail loudly — it
// would find no socket, fall back to TCP, and quietly lose the verified
// identity that is the whole feature. Resolving only the default was exactly
// that drift for every operator who had overridden it (knomit#271).
//
// It is deliberately NOT Load. Load runs Validate and parses every other
// KNOMIT_* variable, so a problem unrelated to the socket — or a bridge
// launched with a different environment from the server's — would turn into
// an error here, and the bridge would drop to TCP for a reason that has
// nothing to do with where the listener is.
//
// A knomit.toml that cannot be decoded is an error, not a fall-through to the
// default: Load refuses the same file, so the default would be a listener
// nobody opened.
//
// It NO LONGER returns "" on Windows. That absence was phase 1 having no
// credential there at all (knomit#245); the pipe is the credential now, and
// a caller that still treats "" as "Windows" would just keep using TCP.
func SocketPath() (string, error) {
	home, path, err := homeAndConfig()
	if err != nil {
		return "", err
	}
	// KNOWN GAP, accepted: findConfigFile looks beside os.Executable() before
	// <home>/knomit.toml, and the bridge is not the server's executable. A
	// knomit.toml beside only one of the two binaries is read by that one
	// alone. They ship side by side (dist/, the .app), so in practice both see
	// the same file; <home>/knomit.toml is read by both wherever they live.
	var fromTOML Config
	if path != "" {
		if _, err := toml.DecodeFile(path, &fromTOML); err != nil {
			return "", err
		}
	}
	return socketFor(home, fromTOML.Socket, os.Getenv("KNOMIT_SOCKET")), nil
}

// socketFor is the ONE place the local listener is decided from its inputs:
// fromEnv (KNOMIT_SOCKET), else fromTOML (the knomit.toml `socket` key), else
// this platform's default under home. Load and SocketPath both call it, so
// the server and the bridge cannot order the layers differently. It is pure:
// callers read the environment and pass it in.
//
// home must already be tilde-expanded. The chosen value itself is NOT expanded
// — Load never has expanded Socket, and doing it on one side only would be a
// fresh disagreement.
func socketFor(home, fromTOML, fromEnv string) string {
	switch {
	case fromEnv != "":
		return fromEnv
	case fromTOML != "":
		return fromTOML
	default:
		return localListenerName(home)
	}
}
