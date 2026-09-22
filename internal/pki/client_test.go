package pki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// clientSide installs peer's files in its own dir, as `knomit identity
// install` would on the dialing instance.
func (f fleet) clientSide(t *testing.T, host string) (member, string) {
	t.Helper()
	m := f.enroll(t, host)
	return m, f.install(t, m)
}

func dial(t *testing.T, addr, dir, keyPath string) (Identity, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return Dial(ctx, addr, dir, keyPath)
}

func TestDial_ReturnsTheServersVerifiedIdentity(t *testing.T) {
	f, srvM, _, rec, addr := serverSide(t)
	peer, dir := f.clientSide(t, "peer")
	id, err := dial(t, addr, dir, peer.keyPath)
	if err != nil {
		t.Fatalf("%v\nserver log:\n%s", err, rec)
	}
	if id.Fingerprint != Fingerprint(srvM.pub) || id.Host != "server" || id.Role != RoleInstance {
		t.Fatalf("identity %+v, want server's %s", id, Fingerprint(srvM.pub))
	}
	// And HTTPClient carries the client's identity to the handler.
	c, err := HTTPClient(dir, peer.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Get("https://" + addr + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != Fingerprint(peer.pub) {
		t.Fatalf("server saw %q, want %q", body, Fingerprint(peer.pub))
	}
}

func TestDial_ServerUnderAnotherRootIsUntrusted(t *testing.T) {
	_, _, _, _, addr := serverSide(t) // server enrolled in ITS fleet
	other := newFleet(t)
	peer, dir := other.clientSide(t, "peer")
	if _, err := dial(t, addr, dir, peer.keyPath); !errors.Is(err, ErrUntrustedRoot) {
		t.Fatalf("err=%v, want ErrUntrustedRoot", err)
	}
}

func TestDial_RevokedServerIsRefused(t *testing.T) {
	f, srvM, _, _, addr := serverSide(t)
	peer, dir := f.clientSide(t, "peer")
	if _, err := dial(t, addr, dir, peer.keyPath); err != nil { // positive control
		t.Fatal(err)
	}
	if _, err := Revoke(f.dir, f.root, Revoked{Serial: srvM.cert.SerialNumber, At: time.Now()}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.publishCRL(t, dir) // the CLIENT's CRL now lists the server
	if _, err := dial(t, addr, dir, peer.keyPath); !errors.Is(err, ErrRevoked) {
		t.Fatalf("err=%v, want ErrRevoked", err)
	}
}

// A long-lived HTTPClient picks up a new CRL on its next handshake.
func TestHTTPClient_PicksUpANewCRLWithoutRebuild(t *testing.T) {
	f, srvM, _, _, addr := serverSide(t)
	peer, dir := f.clientSide(t, "peer")
	c, err := HTTPClient(dir, peer.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	c.Transport.(*http.Transport).DisableKeepAlives = true
	if _, err := c.Get("https://" + addr + "/"); err != nil {
		t.Fatal(err)
	}
	f.Revoke(t, srvM, dir)
	if _, err := c.Get("https://" + addr + "/"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("err=%v, want ErrRevoked from the SAME client", err)
	}
}

func TestDial_ServerCertificateWithoutSANIsRefused(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	bare := f.crafted(t, srvM.pub) // chains to the root, wraps the server key, NO SAN
	srvM.certPEM = certPEM(bare.Raw)
	sdir := f.install(t, srvM)
	addr := startServer(t, sdir, srvM.keyPath, &recorder{})
	peer, dir := f.clientSide(t, "peer")
	if _, err := dial(t, addr, dir, peer.keyPath); !errors.Is(err, ErrSANMissing) {
		t.Fatalf("err=%v, want ErrSANMissing", err)
	}
}

// With InsecureSkipVerify, crypto/tls ignores RootCAs, so the fleet-only
// guarantee is VerifyInstanceChain's pool. The server here is enrolled under
// a "public" root; the client belongs to a DIFFERENT fleet but has that public
// root stuffed into its RootCAs — as a well-meaning "add the system pool" edit
// would. The server is still refused: RootCAs decides nothing.
func TestClientConfig_RootCAsIsInertTheFleetRootAloneDecides(t *testing.T) {
	public := newFleet(t)
	srvM := public.enroll(t, "server")
	addr := startServer(t, public.install(t, srvM), srvM.keyPath, &recorder{})

	f := newFleet(t)
	peer, dir := f.clientSide(t, "peer")
	cfg, err := ClientConfig(dir, peer.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(public.root.Cert)
	cfg.RootCAs = pool
	c, err := tls.Dial("tcp", addr, cfg)
	if err == nil {
		c.Close()
		t.Fatal("a server under a root that is only in RootCAs was accepted")
	}
	if !errors.Is(err, ErrUntrustedRoot) {
		t.Fatalf("err=%v, want ErrUntrustedRoot", err)
	}
	// Positive control: a client of the PUBLIC fleet reaches the same server,
	// so the refusal above is the fleet root's doing, not a broken server.
	pubPeer, pubDir := public.clientSide(t, "pub-peer")
	if _, err := dial(t, addr, pubDir, pubPeer.keyPath); err != nil {
		t.Fatalf("the server's own fleet cannot reach it: %v", err)
	}
}

func TestClientConfig_FailsClosedAtConstruction(t *testing.T) {
	f := newFleet(t)
	peer, dir := f.clientSide(t, "peer")
	if _, err := ClientConfig(dir, peer.keyPath); err != nil { // positive control
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, CRLFile))
	if _, err := ClientConfig(dir, peer.keyPath); !errors.Is(err, ErrCRLMissing) {
		t.Fatalf("no CRL: err=%v, want ErrCRLMissing", err)
	}
	_, dir2 := f.clientSide(t, "peer2")
	os.Remove(filepath.Join(dir2, RootCertFile))
	if _, err := ClientConfig(dir2, peer.keyPath); err == nil {
		t.Fatal("no root: constructed")
	}
	other := f.enroll(t, "other")
	_, dir3 := f.clientSide(t, "peer3")
	if _, err := ClientConfig(dir3, other.keyPath); err == nil {
		t.Fatal("certificate for another key: constructed")
	}
}

// TLS 1.3 lets the CLIENT finish its handshake before the server has judged
// the client certificate; the server's refusal arrives as an alert on the
// first read. Dial must not report "ok" for a peer that turned us away.
// Found by the manual two-instance run: a revoked client was told "dial ok".
func TestDial_ServerRefusingUsIsAnErrorNotOK(t *testing.T) {
	f, _, sdir, rec, addr := serverSide(t)
	peer, dir := f.clientSide(t, "peer")
	if _, err := dial(t, addr, dir, peer.keyPath); err != nil { // positive control
		t.Fatal(err)
	}
	f.Revoke(t, peer, sdir) // the SERVER's CRL lists us; ours does not list it
	_, err := dial(t, addr, dir, peer.keyPath)
	if !errors.Is(err, ErrRefusedByPeer) {
		t.Fatalf("err=%v, want ErrRefusedByPeer\nserver log:\n%s", err, rec)
	}
	if !rec.has(ErrRevoked.Error()) {
		t.Fatalf("server did not log why:\n%s", rec)
	}
}
