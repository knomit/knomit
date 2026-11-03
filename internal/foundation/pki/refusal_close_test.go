package pki

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
)

// A client that is slow between its handshake and its request (a loaded
// machine) is how a refusal turned into a reset on Windows (knomit#543): the
// server closed at once with the request unread, the RST followed the alert,
// and Windows dropped the unread alert. The delay makes that deterministic.
const slowClientDelay = 20 * time.Millisecond

// setAfterHandshake installs the client-side test seam for one test.
func setAfterHandshake(t *testing.T, f func()) {
	t.Helper()
	prev := afterHandshakeHook.Swap(&f)
	t.Cleanup(func() { afterHandshakeHook.Store(prev) })
}

func slowClient() { time.Sleep(slowClientDelay) }

// startRefusalServer serves x.repo as B, counting connections, on a listener
// chosen by wrap: NewListener (production), or tls.NewListener alone (the
// close every knomit before #543 had, and an old host still has).
func startRefusalServer(t *testing.T, x *twoInstances, wrap func(net.Listener, *tls.Config) net.Listener) (string, *atomic.Int32) {
	t.Helper()
	cfg, err := ServerConfig(x.bDir, x.b.keyPath, (&recorder{}).logf)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &atomic.Int32{}
	srv := &http.Server{Handler: gitHandler(x.repo), ErrorLog: discardLogger()}
	go srv.Serve(wrap(countingListener{ln, n}, cfg))
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String(), n
}

func oldClose(ln net.Listener, cfg *tls.Config) net.Listener { return tls.NewListener(ln, cfg) }

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
		return l.refusalListener.Listener.Accept()
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
// to a slow client, on the FIRST connection: no redial was needed.
// SABOTAGE: refusalConn.close always closing hard -> red on Windows
// (WSAECONNABORTED at the first read).
func TestClassifyOrigin_ARefusalReachesASlowClientAsTheAlert(t *testing.T) {
	x := newTwoInstances(t)
	x.f.revoke(t, x.a, x.bDir) // B's CRL now lists A
	setAfterHandshake(t, slowClient)
	const rounds = 10
	for i := 0; i < rounds; i++ {
		peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, peerURL(x.addr))
		if peer || !errors.Is(err, ErrRefusedByPeer) || !strings.Contains(err.Error(), "tls: bad certificate") {
			t.Fatalf("round %d: peer=%v err=%v, want ErrRefusedByPeer with the bad_certificate alert", i, peer, err)
		}
	}
	if n := accepts[x.addr].Load(); n != rounds {
		t.Fatalf("%d connections for %d classifications, want one each (the alert must win without a redial)", n, rounds)
	}
}

// THE REDIAL (c): against a host that still closes the old way, the first
// connection resets (the slow client), the one redial meets the fixed close
// and reads the alert.
func TestClassifyOrigin_ARefusalLostToAResetIsRedialledOnce(t *testing.T) {
	windowsOnly(t)
	x := newTwoInstances(t)
	x.f.revoke(t, x.a, x.bDir)
	addr, n := startRefusalServer(t, x, firstOldThenNew)
	setAfterHandshake(t, slowClient)
	peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, peerURL(addr))
	if peer || !errors.Is(err, ErrRefusedByPeer) {
		t.Fatalf("peer=%v err=%v, want ErrRefusedByPeer from the redial", peer, err)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("%d connections, want 2 (the reset one and one redial)", got)
	}
}

// Never more than one redial, and a reset is never named a refusal: against
// a host whose every close resets, the error is the reset, after exactly two
// connections.
func TestClassifyOrigin_ASecondResetIsReturnedNotRedialled(t *testing.T) {
	windowsOnly(t)
	x := newTwoInstances(t)
	x.f.revoke(t, x.a, x.bDir)
	addr, n := startRefusalServer(t, x, oldClose)
	setAfterHandshake(t, slowClient)
	peer, err := ClassifyOrigin(ctx10(t), x.aDir, x.a.keyPath, peerURL(addr))
	if peer || err == nil || errors.Is(err, ErrRefusedByPeer) || errors.Is(err, ErrOriginCertRefused) {
		t.Fatalf("peer=%v err=%v, want the reset itself (no refusal)", peer, err)
	}
	if !isConnReset(err) {
		t.Fatalf("err=%v, want the connection reset/abort", err)
	}
	if got := n.Load(); got != 2 {
		t.Fatalf("%d connections, want exactly 2 (one redial, no more)", got)
	}
}

// The fleet transport's fetch (go-git, info/refs): the slow client against
// the production listener reads the alert; against a host that always resets
// it redials info/refs exactly once and returns the reset, never a refusal.
func TestGitTransport_ASlowClientIsRefusedWithTheAlert(t *testing.T) {
	x := newTwoInstances(t)
	setAfterHandshake(t, slowClient) // before the fleet client is built
	src := x.useA(t)
	clone, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(x.addr)})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	x.f.Revoke(t, x.a, x.bDir)
	for i := 0; i < 5; i++ {
		closeIdle(t, src)
		err = clone.FetchContext(ctx10(t), &git.FetchOptions{})
		if !errors.Is(err, ErrRefusedByPeer) || !strings.Contains(err.Error(), "tls: bad certificate") {
			t.Fatalf("round %d: err=%v, want ErrRefusedByPeer with the bad_certificate alert", i, err)
		}
	}
}

func TestGitTransport_InfoRefsResetIsRedialledExactlyOnce(t *testing.T) {
	windowsOnly(t)
	x := newTwoInstances(t)
	addr, n := startRefusalServer(t, x, oldClose)
	setAfterHandshake(t, slowClient)
	src := x.useA(t)
	clone, err := git.PlainCloneContext(ctx10(t), t.TempDir(), false, &git.CloneOptions{URL: fleetURL(addr)})
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	x.f.Revoke(t, x.a, x.bDir)
	closeIdle(t, src)
	before := n.Load()
	err = clone.FetchContext(ctx10(t), &git.FetchOptions{})
	if err == nil || errors.Is(err, ErrRefusedByPeer) || !isConnReset(err) {
		t.Fatalf("err=%v, want the reset itself (no refusal)", err)
	}
	if got := n.Load() - before; got != 2 {
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
