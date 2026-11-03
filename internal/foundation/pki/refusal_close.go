package pki

import (
	"crypto/tls"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// NewListener is the TLS listener for enrolled peers: tls.NewListener over
// inner, with every raw connection wrapped UNDER the TLS layer so that a
// handshake this server REFUSED delivers its alert (knomit#543).
//
// THE PROBLEM. In TLS 1.3 the client finishes its handshake before the server
// has judged its certificate, so it is already writing its HTTP request when
// VerifyConnection refuses. crypto/tls sends the bad_certificate alert and
// net/http's failed-handshake path closes the socket at once. Closing a socket
// whose receive buffer holds the client's unread request, or receiving that
// request after the close, makes the server's TCP stack answer with RST, and a
// Windows client DISCARDS data it has received but not yet read when the RST
// arrives: the alert is lost and the client's first read fails with
// WSAECONNRESET or WSAECONNABORTED instead. A refusal then no longer reads as
// pki.ErrRefusedByPeer, so a revoked fleet member is not told it is revoked.
//
// THE FIX. A connection whose handshake VerifyConnection refused (marked by
// the Server's GetConfigForClient) closes gracefully: CloseWrite (a FIN after
// the alert), then a bounded drain of what the client still sends, then
// Close. The client reads the alert, closes its side, and the drain ends at
// its EOF. Every other connection closes exactly as before, hard; that
// includes the #258 sweep's NetConn().Close() of an established connection,
// which must cut a revoked peer at once.
//
// THE WRAPPER IS UNDER TLS, so net/http still receives a *tls.Conn and r.TLS
// is still set (a wrapper OVER the *tls.Conn would leave r.TLS nil; see
// auth.ListenTLS).
//
// DoS BOUND. A lingering close costs one goroutine and one socket for at most
// lingerTimeout, reading at most lingerMaxBytes. At most lingerSlots refused
// connections linger at once per listener; past that a refused connection
// closes hard, as before #543, so a flood of refused handshakes cannot pile up
// goroutines. Each one already cost the peer a full TLS handshake.
func NewListener(inner net.Listener, cfg *tls.Config) net.Listener {
	return tls.NewListener(&refusalListener{Listener: inner, slots: make(chan struct{}, lingerSlots)}, cfg)
}

// The lingering close's bounds: see NewListener. One second is several
// round-trips on any real path; the client needs only to read one alert.
const (
	lingerSlots    = 64
	lingerTimeout  = time.Second
	lingerMaxBytes = 64 << 10
)

type refusalListener struct {
	net.Listener
	slots chan struct{}
}

func (l *refusalListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &refusalConn{Conn: c, slots: l.slots}, nil
}

// refusalConn is one raw connection under the TLS layer.
type refusalConn struct {
	net.Conn
	slots   chan struct{}
	refused atomic.Bool
	once    sync.Once
}

// markRefused records that this connection's handshake was refused by our
// verifier, so Close delivers the alert before closing.
func (c *refusalConn) markRefused() { c.refused.Store(true) }

func (c *refusalConn) Close() error {
	var err error
	c.once.Do(func() { err = c.close() })
	return err
}

func (c *refusalConn) close() error {
	cw, ok := c.Conn.(interface{ CloseWrite() error })
	if !c.refused.Load() || !ok {
		return c.Conn.Close()
	}
	select {
	case c.slots <- struct{}{}:
	default:
		return c.Conn.Close() // every slot busy: close hard, as before #543
	}
	if err := cw.CloseWrite(); err != nil {
		<-c.slots
		return c.Conn.Close()
	}
	go func() {
		defer func() { <-c.slots }()
		_ = c.Conn.SetReadDeadline(time.Now().Add(lingerTimeout))
		_, _ = io.Copy(io.Discard, io.LimitReader(c.Conn, lingerMaxBytes))
		_ = c.Conn.Close()
	}()
	return nil
}

// markRefusalOn makes cfg's VerifyConnection mark conn refused when it
// refuses, so conn's Close lingers. conn is the raw connection the handshake
// runs on (ClientHelloInfo.Conn); one not from NewListener is left alone.
func markRefusalOn(cfg *tls.Config, conn net.Conn) {
	rc, ok := conn.(*refusalConn)
	if !ok || cfg.VerifyConnection == nil {
		return
	}
	verify := cfg.VerifyConnection
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		err := verify(cs)
		if err != nil {
			rc.markRefused()
		}
		return err
	}
}
