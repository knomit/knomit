//go:build windows

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"strings"
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

// pipeNamePrefix is the Windows pipe namespace prefix plus knomit's own
// marker. The namespace half is spelled again as internal/auth.PipePrefix,
// which is what RECOGNISES a pipe path when opening or dialling one; this is
// what BUILDS it. TestSocketPath_IsOpenableAndDialableByAuth pins the two
// together — by round-tripping a real listener through both and reading a peer
// off it, not by comparing constants — so the two spellings of an OS constant
// cannot drift apart unnoticed. They are apart at all because internal/auth has no
// knomit dependencies by design, and making config import it — or it import
// config — to share nine characters would invert that.
const pipeNamePrefix = `\\.\pipe\knomit-`

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
