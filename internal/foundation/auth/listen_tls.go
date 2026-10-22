package auth

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
)

// ListenTLS opens the instance-to-instance listener (F19 phase 2): a plain
// TCP listener wrapped by tls.NewListener and NOTHING ELSE.
//
// It must return tls.NewListener's listener unwrapped. net/http recognises a
// TLS connection only by the concrete assertion c.rwc.(*tls.Conn)
// (net/http/server.go, conn.serve), so any wrapper type around the conn
// leaves r.TLS nil on every request, and AuthMiddleware would refuse every
// enrolled peer. Which listener a request came in on is told by the SERVER
// instead: the TLS listener gets its own http.Server whose ConnContext is
// TLSConnContext.
//
// The returned cleanup closes the listener; it is safe to call twice.
func ListenTLS(addr string, cfg *tls.Config) (net.Listener, func(), error) {
	noop := func() {}
	if cfg == nil {
		return nil, noop, fmt.Errorf("tls listen %s: no tls.Config", addr)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, noop, fmt.Errorf("tls listen %s: %w", addr, err)
	}
	tl := tls.NewListener(ln, cfg)
	return tl, func() { _ = tl.Close() }, nil
}

type tlsListenerKey struct{}

// TLSConnContext is the http.Server.ConnContext hook of the TLS listener's
// OWN server. It marks every connection unconditionally: that server serves
// nothing but the TLS listener, so every connection it accepts came in on it.
// The plaintext server keeps ConnContext.
func TLSConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, tlsListenerKey{}, true)
}

// IsTLSListener reports whether the request arrived on the TLS listener.
// r.TLS != nil is NOT this signal: it stops being unique the moment a second
// TLS listener exists (phase 3), and the TLS listener's rule — never
// anonymous, reads included — must not leak to one that serves tokens.
func IsTLSListener(ctx context.Context) bool {
	v, _ := ctx.Value(tlsListenerKey{}).(bool)
	return v
}

// InstancePrincipal is the principal of a verified instance certificate. id
// is the FULL 64-hex fingerprint (pki.Fingerprint), never the 8-hex prefix.
func InstancePrincipal(fp string) Principal {
	return Principal{Kind: KindInstance, ID: fp, Via: ViaCert}
}

// OperatorPrincipal is the principal of a verified operator-role
// certificate. It is NOT an instance: it gets no implicit read (CertGrants)
// and holds only what grants rows give it.
func OperatorPrincipal(fp string) Principal {
	return Principal{Kind: KindOperator, ID: fp, Via: ViaCert}
}
