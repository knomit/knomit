package pki

// knomit#258: revocation reaches connections that are ALREADY established.
//
// The invariant under test: a connection stays open only while a fresh
// handshake from the same peer would be accepted, checked at most one tick
// late (and at once when the connection is first tracked). "Would be
// accepted" is VerifyInstanceChain against the current snapshot, the same
// check VerifyConnection runs, so the invariant cuts on revocation, on the
// peer's certificate expiring mid-connection, and on `install --replace-root`
// moving this instance to another fleet. The first two are tested here; the
// third follows from the same check and needs no path of its own.
//
// Every test here talks HTTP/1.1 by hand over a raw *tls.Conn. http.Transport
// silently retries an idempotent request on a FRESH connection when a reused
// one fails, which would let a new handshake do the refusing and make the
// sweep look like it worked when it did not run.
//
// SABOTAGE CHECKS, run against 6d940317 (rerun after touching Server,
// ConnRegistry or configFor). Each mutation, and the tests it turned red:
//   - ConnRegistry.Sweep returns 0: KeepAlive, OneTick, Expiry, and
//     TestConnRegistry_SweepClosesOnlyTheConnsTheCheckRefuses.
//   - Server.tick refreshes without sweeping: KeepAlive, OneTick, Expiry.
//   - Server.tick sweeps only when nothing was adopted (a bug this file
//     caught during development): OneTick only — every multi-tick test
//     still passes, which is why OneTick exists.
//   - Run never ticks: KeepAlive, ConnActiveOnlyAfterAdoption, Expiry.
//     KeepAlive counts ClientHellos after the CRL write and requires zero,
//     so no handshake can be what adopted the CRL.
//   - ConnRegistry.ConnState does not judge on first track:
//     ConnActiveOnlyAfterAdoption, and
//     TestConnRegistry_ChecksAConnectionWhenItIsFirstTracked.
//   - The registry's check narrowed to IsRevoked: Expiry.
//   - Server.refresh (the handshake path) no longer sweeps after adopting:
//     HandshakeThatAdoptsTheCRLCutsAtOnce only (review F2 found it untested;
//     added in the fix push, sabotage run against its commit).

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// sweepServer is a real TLS listener built from NewServer, wired as
// cmd/serve_tls.go wires it: ConnState from the Server, Run on a ticker.
type sweepServer struct {
	s      *Server
	addr   string
	hellos *atomic.Int64 // ClientHellos seen: every handshake starts with one
	rec    *recorder
}

func startSweepServer(t *testing.T, dir, keyPath string, interval time.Duration) *sweepServer {
	t.Helper()
	rec := &recorder{}
	s, err := NewServer(dir, keyPath, rec.logf, interval)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.TLSConfig().Clone()
	hellos := &atomic.Int64{}
	inner := cfg.GetConfigForClient
	cfg.GetConfigForClient = func(h *tls.ClientHelloInfo) (*tls.Config, error) {
		hellos.Add(1)
		return inner(h)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, "ok")
		}),
		ConnState: s.ConnState,
		ErrorLog:  discardLogger(),
	}
	go srv.Serve(tls.NewListener(ln, cfg))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done; srv.Close() })
	return &sweepServer{s: s, addr: ln.Addr().String(), hellos: hellos, rec: rec}
}

// rawConn is one kept-alive HTTP/1.1 connection: one handshake, then as many
// requests as the test writes on it.
type rawConn struct {
	c  *tls.Conn
	br *bufio.Reader
}

func (f fleet) dialRaw(t *testing.T, m member, addr string, nextProtos ...string) *rawConn {
	t.Helper()
	cfg := f.clientFor(t, m, nil).Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg.NextProtos = nextProtos
	c, err := tls.Dial("tcp", addr, cfg)
	if err != nil {
		t.Fatalf("dial as %s: %v", m.cert.Subject.CommonName, err)
	}
	t.Cleanup(func() { c.Close() })
	return &rawConn{c: c, br: bufio.NewReader(c)}
}

// get sends one request on the SAME connection and reads its response.
func (rc *rawConn) get() error {
	rc.c.SetDeadline(time.Now().Add(5 * time.Second))
	defer rc.c.SetDeadline(time.Time{})
	if _, err := io.WriteString(rc.c, "GET / HTTP/1.1\r\nHost: knomit\r\n\r\n"); err != nil {
		return err
	}
	resp, err := http.ReadResponse(rc.br, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "ok" {
		return fmt.Errorf("status %d body %q", resp.StatusCode, body)
	}
	return nil
}

// cutWithin reports whether the server closes this connection within d,
// without the client sending anything: a read ends in EOF or a reset.
func (rc *rawConn) cutWithin(d time.Duration) bool {
	rc.c.SetReadDeadline(time.Now().Add(d))
	defer rc.c.SetReadDeadline(time.Time{})
	_, err := rc.br.ReadByte()
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return err != nil
}

func (f fleet) revoke(t *testing.T, m member, instDir string) {
	t.Helper()
	if _, err := Revoke(f.dir, f.root, Revoked{Serial: m.cert.SerialNumber, At: time.Now()}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.publishCRL(t, instDir)
}

func TestServer_RevocationCutsTheSameKeepAliveConnWithoutAHandshake(t *testing.T) {
	const tick = 100 * time.Millisecond
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	a, b := f.enroll(t, "peer-a"), f.enroll(t, "peer-b")
	ss := startSweepServer(t, dir, srvM.keyPath, tick)

	ca, cb := f.dialRaw(t, a, ss.addr), f.dialRaw(t, b, ss.addr)
	for _, rc := range []*rawConn{ca, cb} {
		if err := rc.get(); err != nil {
			t.Fatalf("before revocation: %v\n%s", err, ss.rec)
		}
	}
	hellosBefore := ss.hellos.Load()

	f.revoke(t, a, dir)
	if !ca.cutWithin(20 * tick) {
		t.Fatalf("A's established connection was not cut within 20 ticks of the CRL write\n%s", ss.rec)
	}
	// No handshake adopted the CRL: only the ticker can have.
	if n := ss.hellos.Load() - hellosBefore; n != 0 {
		t.Fatalf("%d ClientHello(s) after the CRL write; the cut must not depend on one", n)
	}
	if !ss.rec.has("tls_reloaded") || !ss.rec.has("tls_conn_closed") || !ss.rec.has(ErrRevoked.Error()) {
		t.Fatalf("want a reload and a close naming %v:\n%s", ErrRevoked, ss.rec)
	}
	// Positive control: B's SAME connection still works after the sweep.
	if err := cb.get(); err != nil {
		t.Fatalf("B's kept-alive connection did not survive the sweep: %v\n%s", err, ss.rec)
	}
	// A fresh connection from A is refused by the handshake with the reason.
	cfg := f.clientFor(t, a, nil).Transport.(*http.Transport).TLSClientConfig
	if c, err := tls.Dial("tcp", ss.addr, cfg); err == nil {
		// TLS 1.3: the client's handshake completes before the server judges
		// the certificate; the refusal arrives on first use.
		_, err = io.WriteString(c, "GET / HTTP/1.1\r\nHost: knomit\r\n\r\n")
		if err == nil {
			_, err = http.ReadResponse(bufio.NewReader(c), nil)
		}
		c.Close()
		if err == nil {
			t.Fatal("a fresh connection from revoked A was served")
		}
	}
	if !ss.rec.has("tls_refused") {
		t.Fatalf("the fresh connection's refusal was not logged:\n%s", ss.rec)
	}
}

// The tick that ADOPTS a new CRL must also sweep with it, so one tick after
// the write is enough. The ticker is never allowed to fire (interval one
// hour); the test drives exactly one tick by hand.
func TestServer_OneTickAfterTheCRLWriteCutsTheConn(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	a, b := f.enroll(t, "peer-a"), f.enroll(t, "peer-b")
	ss := startSweepServer(t, dir, srvM.keyPath, time.Hour)

	ca, cb := f.dialRaw(t, a, ss.addr), f.dialRaw(t, b, ss.addr)
	for _, rc := range []*rawConn{ca, cb} {
		if err := rc.get(); err != nil {
			t.Fatalf("before revocation: %v\n%s", err, ss.rec)
		}
	}
	f.revoke(t, a, dir)
	ss.s.tick()
	if !ca.cutWithin(2 * time.Second) {
		t.Fatalf("one tick after the CRL write, A's connection is still open\n%s", ss.rec)
	}
	if err := cb.get(); err != nil {
		t.Fatalf("B's connection did not survive the tick: %v", err)
	}
}

// The fast path (review F2, #268): when a ClientHello is what adopts the new
// CRL, that same adoption cuts established connections at once, not one tick
// later. The ticker never fires (interval one hour) and no tick() is driven:
// the ONLY thing that can adopt the CRL here is B's fresh handshake, the
// mirror image of the keep-alive test, which requires zero hellos.
// Sabotage: deleting the sweep in Server.refresh leaves A open and this fails.
func TestServer_HandshakeThatAdoptsTheCRLCutsAtOnce(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	a, b := f.enroll(t, "peer-a"), f.enroll(t, "peer-b")
	ss := startSweepServer(t, dir, srvM.keyPath, time.Hour)

	ca := f.dialRaw(t, a, ss.addr)
	if err := ca.get(); err != nil {
		t.Fatalf("before revocation: %v\n%s", err, ss.rec)
	}
	hellosBefore := ss.hellos.Load()
	f.revoke(t, a, dir)
	if ss.rec.has("tls_reloaded") {
		t.Fatalf("the CRL was adopted before any handshake; nothing but a ClientHello may adopt it here\n%s", ss.rec)
	}

	cb := f.dialRaw(t, b, ss.addr) // this ClientHello adopts the CRL
	if n := ss.hellos.Load() - hellosBefore; n < 1 {
		t.Fatalf("%d ClientHellos after the CRL write; the test needs B's to adopt it", n)
	}
	if !ca.cutWithin(2 * time.Second) {
		t.Fatalf("the adoption by B's handshake did not cut A's established connection\n%s", ss.rec)
	}
	if !ss.rec.has("tls_reloaded") || !ss.rec.has(ErrRevoked.Error()) {
		t.Fatalf("want the handshake-driven reload and a close naming %v:\n%s", ErrRevoked, ss.rec)
	}
	// Positive control: B, whose handshake did the adopting, is served.
	if err := cb.get(); err != nil {
		t.Fatalf("B after adopting the CRL: %v", err)
	}
}

// D1/E1: a connection that completed its handshake BEFORE the revocation but
// sent nothing until after the new CRL was adopted is judged when it first
// becomes active, not left until a later sweep. The tick is long, so a cut
// inside the window can only be the judgement at first track. This is the
// shape of the in-flight handshake case too: verified under the old
// snapshot, tracked under the new one.
func TestServer_ConnActiveOnlyAfterAdoptionIsJudgedUnderTheNewSnapshot(t *testing.T) {
	const tick = 150 * time.Millisecond
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	a := f.enroll(t, "peer-a")
	ss := startSweepServer(t, dir, srvM.keyPath, tick)

	ca := f.dialRaw(t, a, ss.addr) // handshake done, no request yet: StateNew, untracked
	f.revoke(t, a, dir)
	deadline := time.Now().Add(20 * tick)
	for !ss.rec.has("tls_reloaded") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !ss.rec.has("tls_reloaded") {
		t.Fatalf("the ticker never adopted the CRL:\n%s", ss.rec)
	}
	if err := ca.get(); err == nil {
		t.Fatal("a connection first used after the revocation was adopted got a response")
	}
	if !ss.rec.has("tls_conn_closed") {
		t.Fatalf("no close logged:\n%s", ss.rec)
	}
}

// D2: the sweep's check is the full verification, so a certificate that
// expires while its connection is open loses the connection too.
func TestServer_ExpiryCutsAnEstablishedConn(t *testing.T) {
	const tick = 100 * time.Millisecond
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	short := f.enrollWindow(t, "short", time.Now().Add(-time.Hour), time.Now().Add(1500*time.Millisecond))
	long := f.enroll(t, "long")
	ss := startSweepServer(t, dir, srvM.keyPath, tick)

	cs, cl := f.dialRaw(t, short, ss.addr), f.dialRaw(t, long, ss.addr)
	for _, rc := range []*rawConn{cs, cl} {
		if err := rc.get(); err != nil {
			t.Fatalf("before expiry: %v\n%s", err, ss.rec)
		}
	}
	if !cs.cutWithin(5 * time.Second) {
		t.Fatalf("the expired peer's connection was not cut\n%s", ss.rec)
	}
	if !ss.rec.has(ErrExpired.Error()) {
		t.Fatalf("the close does not name %v:\n%s", ErrExpired, ss.rec)
	}
	if err := cl.get(); err != nil {
		t.Fatalf("the unexpired peer's connection did not survive: %v", err)
	}
}

// D5: ConnState never fires for an ALPN-negotiated HTTP/2 connection, so the
// registry would never see it. The listener must negotiate no ALPN protocol
// even when the client offers h2.
func TestServer_NeverNegotiatesH2(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	a := f.enroll(t, "peer-a")
	ss := startSweepServer(t, dir, srvM.keyPath, time.Hour)

	ca := f.dialRaw(t, a, ss.addr, "h2", "http/1.1")
	if p := ca.c.ConnectionState().NegotiatedProtocol; p != "" {
		t.Fatalf("negotiated %q; the TLS listener must not negotiate ALPN (ConnState skips h2)", p)
	}
	// Positive control that the offer did not simply break the connection.
	if err := ca.get(); err != nil {
		t.Fatalf("HTTP/1.1 over the h2-offering conn: %v", err)
	}
}

// E2: Run returns when its context is cancelled, so the ticker goroutine does
// not outlive the listener.
func TestServer_RunStopsWhenItsContextIsCancelled(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	s, err := NewServer(dir, srvM.keyPath, nil, 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	// Positive control: it is still running before the cancel.
	select {
	case <-done:
		t.Fatal("Run returned before its context was cancelled")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}
