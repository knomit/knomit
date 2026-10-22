package auth

import (
	"context"
	"net"
)

type peerKey struct{}

// Peer is the kernel's answer about who is on the other end of a LOCAL
// connection, already in the shape a Principal wants.
//
// It is a struct with a STRING id rather than the (uid, pid int) pair it
// replaced because the kernel's answer has a different shape per platform: a
// uid is a number, a Windows SID is not. Flattening the Windows case onto an
// int is what produced "uid:-1" from os.Getuid() there (knomit#245, defect
// B) — a principal that names nobody, seeded at boot and never matched by any
// request.
//
// ID is formatted by this package's per-platform localID ("uid:501" on unix,
// "sid:S-1-5-21-…" on Windows) and Via is LocalVia. Both come from the same
// two declarations LocalPrincipal uses for this process's own identity, which
// is what stops boot-time grant seeding and request-time middleware from ever
// disagreeing about what the local user is called.
type Peer struct {
	ID  string
	Via Via
	PID int
}

// Principal is the caller these credentials denote. The middleware calls it;
// app's boot seeding calls LocalPrincipal; local_test.go asserts the two
// agree on this machine, which is the invariant the pair exists for.
func (p Peer) Principal() Principal {
	return Principal{Kind: KindBridge, ID: p.ID, Via: p.Via}
}

// ConnContext is the http.Server.ConnContext hook. It runs once per accepted
// connection, before any request is read, which is the only moment the
// net.Conn is in hand; every request on that connection then inherits the
// kernel's answer through r.Context(). A connection with no peer credentials
// (any TCP connection) leaves the context untouched, so PeerFromContext
// answers false rather than a zero Peer.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	p, ok := PeerCred(c)
	if !ok {
		return ctx
	}
	return WithPeer(ctx, p)
}

// WithPeer attaches a peer credential directly. Production code calls
// ConnContext, which derives it from the kernel; this exists for the edge
// and for tests in other packages, which cannot reach an unexported setter.
func WithPeer(ctx context.Context, p Peer) context.Context {
	return context.WithValue(ctx, peerKey{}, p)
}

// PeerFromContext returns the kernel-reported peer of the local connection,
// or false when the request did not arrive over one.
func PeerFromContext(ctx context.Context) (Peer, bool) {
	p, has := ctx.Value(peerKey{}).(Peer)
	if !has {
		return Peer{}, false
	}
	return p, true
}
