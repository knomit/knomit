package pki

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
)

// A client that is slow between its handshake and its request (a loaded
// machine) is how a refusal turned into a reset on Windows (knomit#543): the
// server closed at once, the RST followed the alert, and Windows dropped the
// unread alert. The tests make that DETERMINISTIC rather than likely: the
// client's seam (afterHandshakeHook) blocks until the server has finished
// closing the refused connection, so the client writes its request only to a
// server that has already closed (the old close: that write is answered with
// RST) or already half-closed and is draining (the fixed close: no RST).

// setAfterHandshake installs the client-side test seam for one test.
func setAfterHandshake(t *testing.T, f func()) {
	t.Helper()
	prev := afterHandshakeHook.Swap(&f)
	t.Cleanup(func() { afterHandshakeHook.Store(prev) })
}

// refusalServer is B serving x.repo, counting connections, and reporting
// each REFUSED connection's close: net/http moves a connection whose
// handshake failed from StateNew straight to StateClosed, and does so only
// after its Close (the old hard close, or NewListener's half-close) returned.
type refusalServer struct {
	addr     string
	accepted *atomic.Int32
	closed   chan struct{} // one per refused connection, after its Close
}

// waitClosed is the client seam: block until the server has closed one more
// refused connection. armed == nil means always armed.
func (s *refusalServer) waitClosed(t *testing.T, armed *atomic.Bool) func() {
	return func() {
		if armed != nil && !armed.Load() {
			return
		}
		select {
		case <-s.closed:
		case <-time.After(10 * time.Second):
			t.Error("the server never closed the refused connection")
		}
	}
}

// startRefusalServer serves on a listener chosen by wrap: NewListener
// (production), or tls.NewListener alone (the close every knomit before #543
// had, and an old host still has).
func startRefusalServer(t *testing.T, x *twoInstances, wrap func(net.Listener, *tls.Config) net.Listener) *refusalServer {
	t.Helper()
	cfg, err := ServerConfig(x.bDir, x.b.keyPath, (&recorder{}).logf)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &refusalServer{addr: ln.Addr().String(), accepted: &atomic.Int32{}, closed: make(chan struct{}, 64)}
	var active sync.Map
	srv := &http.Server{
		Handler:  gitHandler(x.repo),
		ErrorLog: discardLogger(),
		ConnState: func(c net.Conn, st http.ConnState) {
			switch st {
			case http.StateActive:
				active.Store(c, true)
			case http.StateClosed:
				if _, ok := active.LoadAndDelete(c); !ok {
					s.closed <- struct{}{}
				}
			}
		},
	}
	go srv.Serve(wrap(countingListener{ln, s.accepted}, cfg))
	t.Cleanup(func() { srv.Close() })
	return s
}

// oldClose is a host without #543's fix: tls.NewListener alone, whose
// refused connection is closed at once. Its close is made an RST
// unconditionally (SO_LINGER 0), which is what the old close produces
// whenever the client's request is unread or arrives after it; forcing it
// keeps the test from depending on which of those the scheduler picked.
func oldClose(ln net.Listener, cfg *tls.Config) net.Listener {
	return tls.NewListener(rstListener{ln}, cfg)
}

type rstListener struct{ net.Listener }

func (l rstListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return rstConn{c}, nil
}

type rstConn struct{ net.Conn }

func (c rstConn) Close() error {
	if tc, ok := c.Conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	return c.Conn.Close()
}

// firstOldThenNew: the first connection closes the old way, every later one
// through NewListener's wrapper, so a redial meets the fixed close.
func firstOldThenNew(ln net.Listener, cfg *tls.Config) net.Listener {
	return tls.NewListener(&firstRaw{refusalListener: refusalListener{Listener: ln, slots: make(chan struct{}, lingerSlots)}}, cfg)
}

type firstRaw struct {
	refusalListener
	n atomic.Int32
}

func (l *firstRaw) Accept() (net.Conn, error) {
	if l.n.Add(1) == 1 {
		return rstListener{l.refusalListener.Listener}.Accept()
	}
	return l.refusalListener.Accept()
}

func windowsOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("the reset that loses the alert, and the redial on it, are Windows-only (isConnReset)")
	}
}

// THE FIX (a): the production listener delivers the refusal as the alert even
// to a client that writes only after the server closed, on the FIRST
// connection: no redial was needed.
// SABOTAGE: refusalConn.close always closing hard -> red on Windows
// (WSAECONNABORTED at the first read).
func TestClassifyOrigin_ARefusalReachesASlowClientAsTheAlert(t *testing.T) {
	x := newTwoInstances(t)
	x.f.revoke(t, x.a, x.bDir) // B's CRL now lists A
	s := startRefusalServer(t, x, NewListener)
	setAfterHandshake(t, s.waitClosed(t, nil))
	const rounds = 10
	for i := 0; i < rounds; i++ {
		peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, peerURL(s.addr))
		if peer || !errors.Is(err, ErrRefusedByPeer) || !strings.Contains(err.Error(), "tls: bad certificate") {
			t.Fatalf("round %d: peer=%v err=%v, want ErrRefusedByPeer with the bad_certificate alert", i, peer, err)
		}
	}
	if n := s.accepted.Load(); n != rounds {
		t.Fatalf("%d connections for %d classifications, want one each (the alert must win without a redial)", n, rounds)
	}
}

// THE REDIAL (c): against a host that still closes the old way, the first
// connection resets, the one redial meets the fixed close and reads the
// alert.
// SABOTAGE: no redial in ClassifyOrigin -> red (the reset is returned).
func TestClassifyOrigin_ARefusalLostToAResetIsRedialledOnce(t *testing.T) {
	windowsOnly(t)
	x := newTwoInstances(t)
	x.f.revoke(t, x.a, x.bDir)
	s := startRefusalServer(t, x, firstOldThenNew)
	setAfterHandshake(t, s.waitClosed(t, nil))
	peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, peerURL(s.addr))
	if peer || !errors.Is(err, ErrRefusedByPeer) {
		t.Fatalf("peer=%v err=%v, want ErrRefusedByPeer from the redial", peer, err)
	}
	if got := s.accepted.Load(); got != 2 {
		t.Fatalf("%d connections, want 2 (the reset one and one redial)", got)
	}
}

// Never more than one redial, and a reset is never named a refusal: against
// a host whose every close resets, the error is the reset, after exactly two
// connections.
// SABOTAGE: no redial -> red (1 connection); a second redial -> red (3).
func TestClassifyOrigin_ASecondResetIsReturnedNotRedialled(t *testing.T) {
	windowsOnly(t)
	x := newTwoInstances(t)
	x.f.revoke(t, x.a, x.bDir)
	s := startRefusalServer(t, x, oldClose)
	setAfterHandshake(t, s.waitClosed(t, nil))
	peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, peerURL(s.addr))
	if peer || err == nil || errors.Is(err, ErrRefusedByPeer) || errors.Is(err, ErrOriginCertRefused) {
		t.Fatalf("peer=%v err=%v, want the reset itself (no refusal)", peer, err)
	}
	if !isConnReset(err) {
		t.Fatalf("err=%v, want the connection reset/abort", err)
	}
	if got := s.accepted.Load(); got != 2 {
		t.Fatalf("%d connections, want exactly 2 (one redial, no more)", got)
	}
}

// gitAgainst clones from s as A (seam disarmed), then revokes A at B and arms
// the seam, so only the refused fetches wait for the server's close.
func gitAgainst(t *testing.T, x *twoInstances, s *refusalServer) (*git.Repository, *fleetSource) {
	t.Helper()
	armed := &atomic.Bool{}
	setAfterHandshake(t, s.waitClosed(t, armed)) // before the fleet client is built
	src := x.useA(t)
	clone, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(s.addr)})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	x.f.Revoke(t, x.a, x.bDir)
	closeIdle(t, src)
	armed.Store(true)
	return clone, src
}

// The fleet transport's fetch (go-git, info/refs) against the production
// listener reads the alert.
// SABOTAGE: refusalConn.close always closing hard -> red on Windows.
func TestGitTransport_ASlowClientIsRefusedWithTheAlert(t *testing.T) {
	x := newTwoInstances(t)
	s := startRefusalServer(t, x, NewListener)
	clone, src := gitAgainst(t, x, s)
	for i := 0; i < 5; i++ {
		closeIdle(t, src)
		err := clone.FetchContext(ctx10(t), &git.FetchOptions{})
		if !errors.Is(err, ErrRefusedByPeer) || !strings.Contains(err.Error(), "tls: bad certificate") {
			t.Fatalf("round %d: err=%v, want ErrRefusedByPeer with the bad_certificate alert", i, err)
		}
	}
}

// Against a host that always resets, the fetch redials info/refs exactly once
// and returns the reset, never a refusal.
// SABOTAGE: no redial in advertisedOnce -> red (1 connection).
func TestGitTransport_InfoRefsResetIsRedialledExactlyOnce(t *testing.T) {
	windowsOnly(t)
	x := newTwoInstances(t)
	s := startRefusalServer(t, x, oldClose)
	clone, _ := gitAgainst(t, x, s)
	before := s.accepted.Load()
	err := clone.FetchContext(ctx10(t), &git.FetchOptions{})
	if err == nil || errors.Is(err, ErrRefusedByPeer) || !isConnReset(err) {
		t.Fatalf("err=%v, want the reset itself (no refusal)", err)
	}
	if got := s.accepted.Load() - before; got != 2 {
		t.Fatalf("%d connections for the fetch, want exactly 2 (one redial of info/refs, no more)", got)
	}
}

// tcpPair is one accepted server-side connection and its client end.
func tcpPair(t *testing.T) (server, client net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	srv, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv, cli
}

func isClosed(c net.Conn) bool { return errors.Is(c.SetDeadline(time.Time{}), net.ErrClosed) }

// Only a refused connection lingers, and only while a slot is free; every
// other close is the hard close it always was (the #258 sweep's included).
func TestRefusalConn_LingersOnlyWhenRefusedAndASlotIsFree(t *testing.T) {
	t.Run("not refused: hard close", func(t *testing.T) {
		raw, _ := tcpPair(t)
		c := &refusalConn{Conn: raw, slots: make(chan struct{}, 1)}
		_ = c.Close()
		if !isClosed(raw) {
			t.Fatal("an unrefused connection was not closed at once")
		}
		if err := c.Close(); !errors.Is(err, net.ErrClosed) {
			t.Fatalf("second Close = %v, want net.ErrClosed", err)
		}
	})
	t.Run("refused: half-close, drain until the client closes, release the slot", func(t *testing.T) {
		raw, cli := tcpPair(t)
		slots := make(chan struct{}, 1)
		c := &refusalConn{Conn: raw, slots: slots}
		c.markRefused()
		_ = c.Close()
		if isClosed(raw) {
			t.Fatal("a refused connection closed at once; it must linger")
		}
		_ = cli.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := cli.Read(make([]byte, 1)); err == nil {
			t.Fatal("the client read data; want the server's FIN (EOF)")
		}
		if _, err := cli.Write([]byte("HEAD / HTTP/1.1\r\n\r\n")); err != nil {
			t.Fatalf("the client's late write failed (%v); the drain must accept it", err)
		}
		cli.Close()
		deadline := time.Now().Add(2 * lingerTimeout)
		for !isClosed(raw) || len(slots) != 0 {
			if time.Now().After(deadline) {
				t.Fatal("the lingering connection was not closed, or its slot not released")
			}
			time.Sleep(5 * time.Millisecond)
		}
	})
	t.Run("refused but every slot busy: hard close", func(t *testing.T) {
		raw, _ := tcpPair(t)
		slots := make(chan struct{}, 1)
		slots <- struct{}{}
		c := &refusalConn{Conn: raw, slots: slots}
		c.markRefused()
		_ = c.Close()
		if !isClosed(raw) {
			t.Fatal("with no slot free a refused connection must close hard")
		}
	})
}
