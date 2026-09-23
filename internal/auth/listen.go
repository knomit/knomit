package auth

import (
	"errors"
	"fmt"
	"net"
)

// ErrSocketInUse means another LIVE process holds the local listener path.
// The caller must not fail on it merely because of that: log at WARN and
// serve TCP only. Taking the path over would silently redirect every bridge
// away from whatever was there first — after phase 1c both `knomit serve` and
// the desktop app open the same path.
//
// It says "another process" and not "another knomit instance", which is all
// that is actually KNOWN. On unix the lock file is knomit's own, so the owner
// almost certainly is knomit; on Windows the pipe namespace is flat and
// world-creatable and ERROR_ACCESS_DENIED is what the OS returns for ANY
// foreign owner, so the name may be held by something with no relation to
// knomit at all.
//
// It is benign ONLY while there is another way in. See RequireLocalListener,
// which both callers consult straight after: with [auth].require = true there
// is no anonymous path, and carrying on here would serve nothing but 403s.
var ErrSocketInUse = errors.New("local listener path is held by another live process")

// ErrPathTooLong means the local listener's path does not fit in a unix
// socket address (sun_path): 104 bytes on darwin, 108 on linux, and neither
// number is typed anywhere (see SunPathCap). It is checked BEFORE anything is
// created, so the caller gets this name rather than a raw EINVAL from
// net.Listen. Callers treat it like ErrSocketInUse: WARN naming path and cap,
// serve TCP only (knomit#253). It is declared on every platform so callers
// compile everywhere; only the unix implementation returns it — a Windows pipe
// name has no such cap.
var ErrPathTooLong = errors.New("local listener path is too long for a unix socket address")

// ErrUnsafeSocketDir means the SHARED fallback directory for the local
// listener (FallbackSocketDir, under the literal /tmp) is not a directory the
// current user owns with mode 0700: missing that, anyone who can write /tmp
// could squat the lock or listen in the server's place and receive the
// bridge's traffic. Neither ListenLocal nor DialLocal will use such a
// directory. Callers treat it like ErrSocketInUse. Unix only, like
// ErrPathTooLong.
var ErrUnsafeSocketDir = errors.New("local listener directory is not private to this user")

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
//     to clear up, and winio.ListenPipe creates the first instance with the
//     FILE_CREATE disposition, so a second listener on a live name is refused
//     by the OS. There is no lock file on Windows and nothing for one to guard.
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

// RequireLocalListener refuses a boot that would answer every request 403.
//
// It is the SECOND DOOR on the property app.checkLocalListener guards, and it
// exists because that one cannot see this. checkLocalListener asks whether a
// local listener is CONFIGURED, at config time; this asks whether one was
// actually BOUND, after ListenLocal has run. The gap between them is
// ErrSocketInUse, which both callers treat as benign — warn, serve TCP only,
// carry on — because a second instance must not steal the first one's
// listener. With [auth].require = true that benign path produces exactly the
// server Defect A exists to prevent: up, healthy-looking, and refusing
// everything, which is harder to diagnose than a server that did not start.
//
// knomit#245 is what made it reachable. Before it, Windows had no socket
// default, so require = true failed at checkLocalListener and never got here;
// now cfg.Socket is always non-empty there, that check always passes, and the
// pipe namespace is flat and world-creatable — ANY process holding the name
// yields ERROR_ACCESS_DENIED, which isPipeNameTaken maps to ErrSocketInUse.
// No attacker is needed.
//
// It takes listenErr so the refusal can say WHY there is no listener rather
// than only that there is none.
func RequireLocalListener(require bool, ln net.Listener, path string, listenErr error) error {
	if !require || ln != nil {
		return nil
	}
	if listenErr != nil {
		return fmt.Errorf("[auth].require = true but this process bound no local authenticated listener at %s: %w. "+
			"Every request would be refused. Free that path, or set [auth].require = false", path, listenErr)
	}
	return fmt.Errorf("[auth].require = true but this process bound no local authenticated listener at %q: "+
		"every request would be refused. Configure one, or set [auth].require = false", path)
}
