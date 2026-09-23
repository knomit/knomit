package pki

import (
	"crypto/ed25519"
	"crypto/tls"
	"net"
	"net/http"
	"sync"
)

// ConnRegistry tracks the TLS listener's established connections so a newly
// adopted CRL can reach them (knomit#258). Without it, verification happens
// only in the handshake: a peer revoked while holding a keep-alive connection
// keeps using it for as long as it keeps talking.
//
// It is fed by the listener's http.Server.ConnState hook. That hook sees the
// *tls.Conn itself, because auth.ListenTLS hands net/http tls.NewListener's
// connections unwrapped (see its doc: a wrapper would leave r.TLS nil).
//
// HTTP/2 would be invisible here: net/http marks an ALPN-negotiated h2
// connection active with its hooks skipped, so ConnState never fires for it.
// The listener negotiates no ALPN protocol (configFor sets no NextProtos), and
// TestServer_NeverNegotiatesH2 pins that. Adding "h2" there without moving
// tracking to the listener would silently exempt every connection from
// revocation.
type ConnRegistry struct {
	check func(tls.ConnectionState) error
	logf  Logf

	mu    sync.Mutex
	conns map[*tls.Conn]struct{}
}

// NewConnRegistry returns an empty registry that judges connections with
// check. check must be the listener's own verifier against its CURRENT
// snapshot, never a narrower test, so that a connection stays open exactly
// while a fresh handshake from the same peer would be accepted. A nil logf
// discards the close lines.
func NewConnRegistry(check func(tls.ConnectionState) error, logf Logf) *ConnRegistry {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &ConnRegistry{check: check, logf: logf, conns: map[*tls.Conn]struct{}{}}
}

// ConnState is an http.Server.ConnState hook.
//
// A connection is tracked from its first StateActive, not StateNew: net/http
// reports StateNew BEFORE the TLS handshake runs (Serve, then conn.serve
// handshakes), so at StateNew there is no peer certificate to judge. By the
// first StateActive net/http has completed the handshake and read request
// bytes. HandshakeComplete is checked anyway, so a caller that reports states
// in another order cannot make Sweep judge a connection with no certificate.
//
// A connection is JUDGED when it is first tracked, against the snapshot
// current then. Its handshake may have been verified under an older one: a
// handshake in flight while a new CRL was adopted, or one that completed and
// then sent nothing (StateNew, untracked) until after the adoption's sweep.
// Either would otherwise escape until the next sweep. It is inserted BEFORE
// it is judged, so a sweep running concurrently either sees it or the
// judgement sees the newer snapshot; both closing it is harmless.
//
// StateHijacked forgets the connection, because net/http never reports a
// hijacked connection closed and keeping it would leak the entry. No knomit
// handler hijacks today; one that does takes its connections out of reach of
// revocation, and must re-verify them itself.
func (r *ConnRegistry) ConnState(c net.Conn, s http.ConnState) {
	tc, ok := c.(*tls.Conn)
	if !ok {
		return
	}
	switch s {
	case http.StateActive, http.StateIdle:
		if !tc.ConnectionState().HandshakeComplete {
			return
		}
		r.mu.Lock()
		_, known := r.conns[tc]
		r.conns[tc] = struct{}{}
		r.mu.Unlock()
		if !known {
			r.judge(tc)
		}
	case http.StateClosed, http.StateHijacked:
		r.mu.Lock()
		delete(r.conns, tc)
		r.mu.Unlock()
	}
}

// Sweep closes every tracked connection that check refuses, logs each with
// the refusal (which carries its named error), and returns how many it closed.
//
// It closes the UNDERLYING connection, not the *tls.Conn: tls.Conn.Close
// first sends close_notify under the same lock a handler's in-progress Write
// holds, so a revoked peer that stops reading could stall the sweep for the
// alert's write deadline. Closing the socket unblocks the handler's reads and
// writes at once; net/http then reports StateClosed.
func (r *ConnRegistry) Sweep() int {
	r.mu.Lock()
	conns := make([]*tls.Conn, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()

	closed := 0
	for _, c := range conns {
		if r.judge(c) {
			closed++
		}
	}
	return closed
}

// judge closes c and forgets it if check refuses it, reporting whether it did.
func (r *ConnRegistry) judge(c *tls.Conn) bool {
	cs := c.ConnectionState()
	if !cs.HandshakeComplete || len(cs.PeerCertificates) == 0 {
		return false
	}
	err := r.check(cs)
	if err == nil {
		return false
	}
	// Logged BEFORE the close, so the line exists by the time the peer can
	// observe the connection gone.
	leaf := cs.PeerCertificates[0]
	kv := []any{"event", "tls_conn_closed", "serial", leaf.SerialNumber.Text(16), "remote", c.RemoteAddr().String()}
	if pub, ok := leaf.PublicKey.(ed25519.PublicKey); ok {
		kv = append(kv, "peer", Fingerprint(pub))
	}
	r.logf(err.Error(), kv...)
	_ = c.NetConn().Close()
	r.mu.Lock()
	delete(r.conns, c)
	r.mu.Unlock()
	return true
}

// len is the number of tracked connections (tests only).
func (r *ConnRegistry) len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}
