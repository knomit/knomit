//go:build !windows

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"knomit/internal/auth"
	"knomit/internal/platform/userdirs"
)

// defaultHome is ~/.knomit on every non-Windows platform.
//
// Unchanged, and deliberately so: the Windows default moved under
// %LOCALAPPDATA% because Windows support had not shipped and there was no
// installed base. macOS and Linux both have one. Moving them to
// ~/Library/Application Support or $XDG_DATA_HOME is a separate decision with
// a migration attached, and this is not it.
//
// Note it hangs off the HOME directory, not off userdirs.StateDir() as Windows
// does — which is exactly the policy this file exists to hold.
func defaultHome() (string, error) {
	home, err := userdirs.HomeDir()
	if err != nil {
		return "", withHomeHint(err)
	}
	return filepath.Join(home, "."+appDir), nil
}

// socketFile is the unix socket's name inside the data root. One spelling,
// for the same reason appDir has one: `knomit serve` opens it and `kb` dials
// it, and a disagreement means the bridge silently falls back to TCP.
const socketFile = "knomit.sock"

// localListenerName is the path of the local authenticated listener for a
// given data root. On unix that is a socket file inside the root, so the
// 0700 root above it is what guards it. internal/platform/privdir.Ensure
// makes it so at boot: it creates the root 0700, or tightens an existing
// wider root this user owns; a root owned by someone else (or a symlink) is
// only warned about, and then the socket is as exposed as the root.
//
// UNLESS that path does not fit in a socket address (knomit#253): a data root
// long enough that <root>/knomit.sock reaches auth.SunPathCap would make
// every listen fail with EINVAL. Such a root gets
// auth.FallbackSocketDir()/<first 8 hex of sha256(filepath.Clean(root))>.sock
// instead — /tmp/knomit-<euid>/…, about 30 bytes. The server (Load) and the
// bridge (SocketPath) both come through here, and the result depends on the
// root and the euid ONLY, never on TMPDIR or XDG_RUNTIME_DIR, so a bridge
// with a different environment still computes the server's path. The root is
// cleaned before hashing so two spellings of one directory agree (the
// Windows pipe name normalises for the same reason). /tmp is shared: auth
// uses that directory only when this user owns it with mode 0700.
//
// The Windows half cannot do this, because a named pipe does not live in the
// filesystem — hence one function per platform rather than a filepath.Join at
// each call site.
func localListenerName(home string) string {
	p := filepath.Join(home, socketFile)
	if len(p) < auth.SunPathCap() {
		return p
	}
	sum := sha256.Sum256([]byte(filepath.Clean(home)))
	return filepath.Join(auth.FallbackSocketDir(), hex.EncodeToString(sum[:])[:8]+".sock")
}

// checkExplicitSocket is the unix rule for an operator-named socket: a leading
// ~ is expanded, and what remains must be an absolute path. A relative one
// would resolve against each process's own working directory.
func checkExplicitSocket(sock string) (string, error) {
	if err := expandTilde(&sock); err != nil {
		return "", err
	}
	if !filepath.IsAbs(sock) {
		return "", fmt.Errorf("socket %q must be an absolute path; set KNOMIT_SOCKET or the knomit.toml socket key to one", sock)
	}
	return sock, nil
}
