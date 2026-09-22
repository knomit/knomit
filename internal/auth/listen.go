package auth

import (
	"errors"
	"net"
)

// ErrSocketInUse means another knomit instance is ALIVE on the local listener
// path. The caller must not fail on it: log at WARN and serve TCP only.
// Taking the path over would silently redirect every bridge away from the
// instance that was there first — after phase 1c both `knomit serve` and the
// desktop app open the same path.
var ErrSocketInUse = errors.New("local socket is in use by a live knomit instance")

// ListenLocal is the ONE place the local authenticated listener is opened.
// Both binaries that serve knomit — `knomit serve` (cmd/serve.go) and the
// desktop app (tools/desktop/boot.go) — call it, because the desktop builds
// its own http.Server and does not pass through cmd/serve.go: phase 1 put the
// socket in cmd/serve.go only, and a desktop-served machine never opened one
// (issue #248).
//
// WHAT the listener is differs by platform, and that is the only thing the
// per-platform halves decide:
//
//   - unix: a domain socket. 0600 on it, under the 0700 data root, IS the
//     credential: the kernel vouches for the peer uid (PeerCred) and the mode
//     decides which uids can reach the socket at all.
//   - windows: a named pipe, whose SDDL gates it in a similar way — this
//     user's SID and SYSTEM, and nobody else (knomit#245). The parallel is
//     not exact: the ACL gates who may OPEN the pipe, not who may CREATE that
//     name, and \\.\pipe\ is world-creatable. See ownerOnlySDDL. A path that
//     is not in the pipe namespace is an ERROR, never a silent nil: opening
//     an ordinary path would create a FILE and the server would look up
//     while accepting nothing.
//
// LIVE OR STALE is never answered by dialling the path — a live listener with
// a full accept backlog refuses exactly like a leftover, so a probe would
// steal a busy server's socket. Each platform answers it with something the
// OS maintains for us, and both map onto ErrSocketInUse:
//
//   - unix: an exclusive flock on <path>.lock, held for the listener's life.
//     The kernel releases it on ANY exit, graceful or not, so "lock held" is
//     exactly "owner alive" and "lock acquired" is exactly "whatever sits at
//     path is a leftover", which is then removed.
//   - windows: the pipe namespace itself. A pipe name exists only while an
//     instance of it is open, so nothing can be left behind for a successor
//     to clear up, and winio.ListenPipe asks for FILE_FLAG_FIRST_PIPE_INSTANCE
//     so a second listener on a live name is refused by the OS. There is no
//     lock file on Windows and there is nothing for one to guard.
//
// The returned cleanup closes the listener and releases whatever the platform
// held; it is safe to call twice. On unix it keeps the lock file reachable, so
// the caller must hold on to it for the listener's life: a dropped cleanup
// lets the GC finalizer close the descriptor and release the lock early.
//
// path == "" returns (nil, noop, nil), so callers need no platform branch.
func ListenLocal(path string) (net.Listener, func(), error) {
	if path == "" {
		return nil, func() {}, nil
	}
	return listenLocal(path)
}
