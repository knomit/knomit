//go:build windows

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"

	"knomit/internal/auth"
)

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

// pipeNamePrefix is the Windows pipe namespace (auth.PipePrefix, which is
// what RECOGNISES a pipe path when opening or dialling one) plus knomit's own
// marker. TestSocketPath_IsOpenableAndDialableByAuth still round-trips a real
// listener through both, reading a peer off it.
const pipeNamePrefix = auth.PipePrefix + "knomit-"

// localListenerName is the path of the local authenticated listener for a
// given data root: a named pipe, because Windows has no credential-carrying
// equivalent of the unix socket. (Windows 10 1803+ does accept
// net.Listen("unix", …), but AF_UNIX there has no SO_PEERCRED, so it could
// vouch for nobody. internal/auth.LocalVia records the consequence.)
//
// The pipe namespace is FLAT and machine-wide, so the name has to CARRY the
// data root rather than sit inside it the way a path does. It is derived from
// the root and from nothing else, so that two knomit instances with different
// roots get different pipes and one with the same root gets the same pipe —
// which is what lets `kb` find `knomit serve` with nothing configured.
//
// The root is cleaned and lower-cased before hashing because Windows paths
// are case-insensitive: C:\Users\x\knomit and c:\users\x\knomit are one
// directory, and a bridge that hashed one while the server hashed the other
// would find no pipe and fall silently back to TCP — the exact drift the
// header of paths.go is about.
//
// 8 bytes of SHA-256 is not a security boundary — the pipe's ACL is
// (internal/auth.ListenLocal) — only a collision boundary between the data
// roots on one machine.
func localListenerName(home string) string {
	norm := strings.ToLower(filepath.Clean(home))
	sum := sha256.Sum256([]byte(norm))
	return pipeNamePrefix + hex.EncodeToString(sum[:8])
}

// checkExplicitSocket is the Windows rule for an operator-named socket: it must
// be a pipe name. auth.ListenLocal and auth.DialLocal refuse anything else, so
// a file path here would pass config and fail at listen time. A tilde is not
// expanded: a `~\` value could only ever be a file.
func checkExplicitSocket(sock string) (string, error) {
	if !strings.HasPrefix(sock, auth.PipePrefix) {
		return "", fmt.Errorf("socket %q must be a pipe name %s<name>; set KNOMIT_SOCKET or the knomit.toml socket key to one",
			sock, auth.PipePrefix)
	}
	return sock, nil
}
