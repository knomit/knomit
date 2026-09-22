package auth

import (
	"context"
	"net"
)

type peerKey struct{}

type peer struct{ uid, pid int }

// ConnContext is the http.Server.ConnContext hook. It runs once per accepted
// connection, before any request is read, which is the only moment the
// net.Conn is in hand; every request on that connection then inherits the
// kernel's answer through r.Context(). A connection with no peer credentials
// (any TCP connection) leaves the context untouched, so PeerFromContext
// answers false rather than uid 0.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	uid, pid, ok := PeerCred(c)
	if !ok {
		return ctx
	}
	return WithPeer(ctx, uid, pid)
}

// WithPeer attaches a peer credential directly. Production code calls
// ConnContext, which derives it from the kernel; this exists for the edge
// and for tests in other packages, which cannot reach an unexported setter.
func WithPeer(ctx context.Context, uid, pid int) context.Context {
	return context.WithValue(ctx, peerKey{}, peer{uid: uid, pid: pid})
}

// PeerFromContext returns the kernel-reported uid and pid of the unix socket
// peer, or false when the request did not arrive over one.
func PeerFromContext(ctx context.Context) (uid, pid int, ok bool) {
	p, has := ctx.Value(peerKey{}).(peer)
	if !has {
		return 0, 0, false
	}
	return p.uid, p.pid, true
}
