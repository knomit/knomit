package pki

import (
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// handshakenPair is one mTLS connection whose handshake has completed, seen
// from both ends: srv is what net/http would hand ConnState, cli is the
// peer's end, used to observe whether srv was closed.
type handshakenPair struct {
	srv *tls.Conn
	cli *tls.Conn
}

// handshaken opens one real TLS connection per member against a server
// config built from dir (the phase 2 reloader, so the server verifies each
// client exactly as the listener does) and returns both ends of each, with
// the handshake complete on both sides.
func handshaken(t *testing.T, f fleet, dir string, srvM member, members ...member) []handshakenPair {
	t.Helper()
	cfg, err := ServerConfig(dir, srvM.keyPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var out []handshakenPair
	for _, m := range members {
		cliCfg := f.clientFor(t, m, nil).Transport.(*http.Transport).TLSClientConfig
		type res struct {
			c   *tls.Conn
			err error
		}
		cliDone := make(chan res, 1)
		go func() {
			c, err := tls.Dial("tcp", ln.Addr().String(), cliCfg)
			cliDone <- res{c, err}
		}()
		raw, err := ln.Accept()
		if err != nil {
			t.Fatal(err)
		}
		srv := tls.Server(raw, cfg)
		if err := srv.Handshake(); err != nil {
			t.Fatalf("server handshake for %s: %v", m.cert.Subject.CommonName, err)
		}
		r := <-cliDone
		if r.err != nil {
			t.Fatalf("client handshake for %s: %v", m.cert.Subject.CommonName, r.err)
		}
		t.Cleanup(func() { srv.Close(); r.c.Close() })
		out = append(out, handshakenPair{srv: srv, cli: r.c})
	}
	return out
}

// closedByPeer reports whether the client end sees its server end gone: a
// read returns EOF or a reset within the deadline, rather than timing out.
func closedByPeer(t *testing.T, c *tls.Conn, within time.Duration) bool {
	t.Helper()
	c.SetReadDeadline(time.Now().Add(within))
	defer c.SetReadDeadline(time.Time{})
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	return err != nil
}

// revokedSerial is a sweep check that fails exactly one certificate's
// serial, standing in for "the new snapshot revokes A".
func revokedSerial(m member) func(tls.ConnectionState) error {
	return func(cs tls.ConnectionState) error {
		if cs.PeerCertificates[0].SerialNumber.Cmp(m.cert.SerialNumber) == 0 {
			return ErrRevoked
		}
		return nil
	}
}

func TestConnRegistry_SweepClosesOnlyTheConnsTheCheckRefuses(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	a, b := f.enroll(t, "peer-a"), f.enroll(t, "peer-b")
	pairs := handshaken(t, f, dir, srvM, a, b)

	// The check admits everyone while the conns are tracked, then refuses A:
	// the same registry seeing a new snapshot adopted.
	var refuseA atomic.Bool
	rec := &recorder{}
	reg := NewConnRegistry(func(cs tls.ConnectionState) error {
		if refuseA.Load() {
			return revokedSerial(a)(cs)
		}
		return nil
	}, rec.logf)
	for _, p := range pairs {
		reg.ConnState(p.srv, http.StateActive)
	}
	if reg.len() != 2 {
		t.Fatalf("tracked %d, want 2", reg.len())
	}
	refuseA.Store(true)
	if n := reg.Sweep(); n != 1 {
		t.Fatalf("Sweep closed %d conns, want 1 (A only)", n)
	}
	if !closedByPeer(t, pairs[0].cli, 2*time.Second) {
		t.Fatal("A's connection is still open after the sweep refused it")
	}
	// Positive control: B was tracked and passed the check, so it must be
	// untouched, and the proof is a round trip on it, not the absence of EOF.
	go pairs[1].srv.Write([]byte("x"))
	pairs[1].cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(pairs[1].cli, make([]byte, 1)); err != nil {
		t.Fatalf("B's connection did not survive the sweep: %v", err)
	}
	if reg.len() != 1 {
		t.Fatalf("registry holds %d conns after the sweep, want 1 (B)", reg.len())
	}
	if !rec.has(ErrRevoked.Error()) || !rec.has("tls_conn_closed") {
		t.Fatalf("the close was not logged with its named reason:\n%s", rec)
	}
	if !rec.has(a.cert.SerialNumber.Text(16)) || !rec.has(Fingerprint(a.pub)) {
		t.Fatalf("the close log does not name A's serial and fingerprint:\n%s", rec)
	}
}

func TestConnRegistry_ForgetsClosedAndHijackedConns(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	a, b := f.enroll(t, "peer-a"), f.enroll(t, "peer-b")
	pairs := handshaken(t, f, dir, srvM, a, b)

	var refuse atomic.Bool
	reg := NewConnRegistry(func(tls.ConnectionState) error {
		if refuse.Load() {
			return ErrRevoked
		}
		return nil
	}, nil)
	reg.ConnState(pairs[0].srv, http.StateActive)
	reg.ConnState(pairs[0].srv, http.StateIdle) // idempotent: still one entry
	reg.ConnState(pairs[1].srv, http.StateActive)
	if reg.len() != 2 {
		t.Fatalf("tracked %d, want 2", reg.len())
	}
	reg.ConnState(pairs[0].srv, http.StateClosed)
	reg.ConnState(pairs[1].srv, http.StateHijacked)
	if reg.len() != 0 {
		t.Fatalf("tracked %d after closed+hijacked, want 0", reg.len())
	}
	// A forgotten conn is not the sweep's to close any more.
	refuse.Store(true)
	if n := reg.Sweep(); n != 0 {
		t.Fatalf("Sweep closed %d forgotten conns", n)
	}
}

// Only a conn whose handshake is COMPLETE carries a peer certificate to
// judge. StateNew fires before the handshake starts, so it must never track;
// a conn reported active before its handshake completed must not be tracked
// either (and must not make Sweep index an empty PeerCertificates).
func TestConnRegistry_TracksOnlyCompletedHandshakes(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	a := f.enroll(t, "peer-a")
	pairs := handshaken(t, f, dir, srvM, a)

	reg := NewConnRegistry(func(tls.ConnectionState) error { return nil }, nil)
	reg.ConnState(pairs[0].srv, http.StateNew)
	if reg.len() != 0 {
		t.Fatal("StateNew was tracked")
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	fresh := tls.Server(c1, &tls.Config{})
	reg.ConnState(fresh, http.StateActive)
	if reg.len() != 0 {
		t.Fatal("a conn whose handshake never ran was tracked")
	}
	// Plain (non-TLS) conns are not the TLS listener's and are ignored.
	reg.ConnState(c2, http.StateActive)
	if reg.len() != 0 {
		t.Fatal("a non-TLS conn was tracked")
	}
	// Positive control: the same registry DOES track the completed one.
	reg.ConnState(pairs[0].srv, http.StateActive)
	if reg.len() != 1 {
		t.Fatal("the completed handshake was not tracked")
	}
}

// Run with -race: ConnState is called from net/http's per-conn goroutines
// while the reloader's ticker sweeps.
func TestConnRegistry_ConcurrentConnStateAndSweepAreRaceFree(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	a, b := f.enroll(t, "peer-a"), f.enroll(t, "peer-b")
	pairs := handshaken(t, f, dir, srvM, a, b)

	reg := NewConnRegistry(func(tls.ConnectionState) error { return nil }, nil)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for range 200 {
				p := pairs[i%2]
				reg.ConnState(p.srv, http.StateActive)
				reg.ConnState(p.srv, http.StateIdle)
			}
		}()
		go func() {
			defer wg.Done()
			for range 200 {
				reg.Sweep()
			}
		}()
	}
	wg.Wait()
	if reg.len() != 2 {
		t.Fatalf("tracked %d after the storm, want 2", reg.len())
	}
}

// E1: a connection is judged the moment it is first tracked, against the
// snapshot current THEN. A handshake verified under the old snapshot that
// becomes active after the adoption's sweep is cut at once, not a tick later.
// Sabotage: dropping the check from ConnState leaves A tracked and open, and
// the first assertion fails.
func TestConnRegistry_ChecksAConnectionWhenItIsFirstTracked(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	a, b := f.enroll(t, "peer-a"), f.enroll(t, "peer-b")
	pairs := handshaken(t, f, dir, srvM, a, b) // both verified: the "old snapshot"

	rec := &recorder{}
	reg := NewConnRegistry(revokedSerial(a), rec.logf) // the "new snapshot" revokes A
	reg.ConnState(pairs[0].srv, http.StateActive)
	reg.ConnState(pairs[1].srv, http.StateActive)
	if !closedByPeer(t, pairs[0].cli, 2*time.Second) {
		t.Fatal("A was tracked under a snapshot that refuses it and left open")
	}
	if reg.len() != 1 {
		t.Fatalf("tracked %d, want 1 (B)", reg.len())
	}
	if !rec.has("tls_conn_closed") || !rec.has(ErrRevoked.Error()) {
		t.Fatalf("the track-time close was not logged with its reason:\n%s", rec)
	}
	// Positive control: B is tracked and alive.
	go pairs[1].srv.Write([]byte("x"))
	pairs[1].cli.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(pairs[1].cli, make([]byte, 1)); err != nil {
		t.Fatalf("B did not survive being tracked: %v", err)
	}
}
