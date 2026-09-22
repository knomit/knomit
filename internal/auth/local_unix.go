//go:build !windows

package auth

import (
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"
)

// LocalVia names the mechanism the local authenticated listener vouches with
// on this platform. On unix that is the domain socket: the kernel answers
// SO_PEERCRED (linux) or LOCAL_PEERPID plus getpeereid (darwin), so nothing
// is stored or presented.
//
// It is a per-platform CONSTANT and not a parameter on purpose. The grants
// key is "<kind>:<id>@<via>", so the server seeding its own row at boot and
// the middleware naming a caller at request time have to pick the same Via or
// the seeded grant will never match the request's principal — a server that
// looks healthy and refuses every local write.
const LocalVia = ViaSocket

// localID formats a kernel-reported local identity as a Principal ID. There
// is exactly one spelling: LocalPrincipal feeds it this process's own uid and
// PeerCred feeds it the peer's, so the two cannot drift.
func localID(uid int) string { return "uid:" + strconv.Itoa(uid) }

// LocalPrincipal is the principal this process presents over the local
// listener: who the OS says we are, named exactly as PeerCred will name us
// when we dial our own socket.
//
// app's boot seeding calls this, and internal/web's AuthMiddleware calls
// Peer.Principal; local_test.go connects this process to itself and asserts
// the two render the same string. That agreement is the whole point of the
// function — seeding a grant for one spelling while the middleware produces
// another is a silent lockout, which is exactly what os.Getuid() did on
// Windows before knomit#245.
//
// The error return is always nil here and exists for the Windows half, which
// has to ask the OS for a token.
func LocalPrincipal() (Principal, error) {
	return Principal{Kind: KindBridge, ID: localID(os.Getuid()), Via: LocalVia}, nil
}

// ListenLocal opens the local authenticated listener at path. One function
// per platform, called by cmd/serve (and callable from the desktop boot), so
// that "what a local listener IS" is answered in one place rather than inline
// at each listen site.
//
// The stale-socket removal and the 0600 mode came from cmd/serve.go and mean
// what they meant there: a socket file outlives an ungraceful exit, and 0600
// on the socket under the 0700 data root IS the credential — the kernel
// vouches for the peer uid, and the file mode decides which uids can reach
// the socket at all. A world-writable socket would let any local user be
// taken for this one.
//
// The returned listener unlinks the socket on Close (net.Listen sets that for
// a path it created), which is what the caller's old deferred os.Remove did.
func ListenLocal(path string) (net.Listener, error) {
	_ = os.Remove(path) // clean up a socket left by an ungraceful exit
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("chmod %s: %w", path, err)
	}
	return l, nil
}

// DialLocal dials the local authenticated listener at path. It is the other
// half of ListenLocal and the bridge's only door to it: keeping both here
// means the client and the server cannot come to disagree about what the
// transport is, the way they once disagreed about where the data root was.
func DialLocal(ctx context.Context, path string, timeout time.Duration) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout}
	return d.DialContext(ctx, "unix", path)
}
