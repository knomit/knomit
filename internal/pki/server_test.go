package pki

// SABOTAGE CHECK (documented negative, required by the plan): under
// RequireAnyClientCert, reloader.verify is the ONLY gate on a client
// certificate. Making it `return nil` must turn EVERY refusal test in this
// file red — self-signed, expired, other root, revoked, resumed-after-revoke,
// lower CRL Number and stale CRL. That was run when this file was written
// (see the commit message); rerun it after touching verify or configFor.

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a thread-safe Logf.
type recorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *recorder) logf(reason string, kv ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, reason+" "+fmt.Sprintf("%v", kv))
}

func (r *recorder) has(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, l := range r.lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func (r *recorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.lines, "\n")
}

// member is one enrolled key: its key file and its certificate PEM.
type member struct {
	keyPath string
	pub     ed25519.PublicKey
	certPEM []byte
	cert    *x509.Certificate
}

func (f fleet) enroll(t *testing.T, host string) member {
	t.Helper()
	keyPath, pub := writeOpenSSHKey(t)
	p, _, err := IssueInstance(f.dir, f.root, pub, host, RoleInstance, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return member{keyPath: keyPath, pub: pub, certPEM: p, cert: parsePEMCert(t, p)}
}

// enrollWindow issues with an explicit validity window (for expired certs).
func (f fleet) enrollWindow(t *testing.T, host string, notBefore, notAfter time.Time) member {
	t.Helper()
	keyPath, pub := writeOpenSSHKey(t)
	serial, _ := randomSerial()
	u, _ := parseURL(SAN(RoleInstance, host, Fingerprint(pub)))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: host},
		NotBefore: notBefore, NotAfter: notAfter,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		URIs:        u,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.root.Cert, pub, f.root.Signer)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return member{keyPath: keyPath, pub: pub, certPEM: certPEM(der), cert: c}
}

// install writes what `knomit identity install` writes: instance.crt,
// root.crt, crl.pem.
func (f fleet) install(t *testing.T, m member) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pki")
	os.MkdirAll(dir, 0o700)
	rootPEM, _ := os.ReadFile(filepath.Join(f.dir, RootCertFile))
	crlPEM, _ := os.ReadFile(filepath.Join(f.dir, CRLFile))
	for name, b := range map[string][]byte{InstanceCertFile: m.certPEM, RootCertFile: rootPEM, CRLFile: crlPEM} {
		if err := writeFileAtomic(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// publishCRL copies the master's current crl.pem to an instance dir, the
// operator's distribution step.
func (f fleet) publishCRL(t *testing.T, instDir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(f.dir, CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(instDir, CRLFile), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// startServer serves an echo of the client's fingerprint over a real TLS
// listener built from ServerConfig.
func startServer(t *testing.T, dir, keyPath string, rec *recorder) string {
	t.Helper()
	cfg, err := ServerConfig(dir, keyPath, rec.logf)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
				http.Error(w, "no TLS state", 500)
				return
			}
			io.WriteString(w, Fingerprint(r.TLS.PeerCertificates[0].PublicKey.(ed25519.PublicKey)))
		}),
		ErrorLog: discardLogger(),
	}
	go srv.Serve(tls.NewListener(ln, cfg))
	t.Cleanup(func() { srv.Close() })
	return ln.Addr().String()
}

// clientFor is a test client presenting m's certificate. It verifies the
// SERVER with VerifyInstanceChain against f's root and CRL — the same
// fail-closed shape as pki.ClientConfig, so no test here carries an
// InsecureSkipVerify without a verifier.
func (f fleet) clientFor(t *testing.T, m member, cache tls.ClientSessionCache) *http.Client {
	t.Helper()
	signer, _, err := LoadSigner(m.keyPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{{Certificate: [][]byte{m.cert.Raw}, PrivateKey: signer, Leaf: m.cert}},
		InsecureSkipVerify: true, // ONLY because VerifyConnection below does the full check
		VerifyConnection: func(cs tls.ConnectionState) error {
			_, err := VerifyInstanceChain(cs.PeerCertificates[0], cs.PeerCertificates[1:], f.root.Cert, f.crl, time.Now(), UsageServer)
			return err
		},
		ClientSessionCache: cache,
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}}
}

func get(c *http.Client, addr string) (string, error) {
	resp, err := c.Get("https://" + addr + "/")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, b)
	}
	return string(b), nil
}

// serverSide is the common setup: a fleet, the server's own enrollment and
// its installed dir.
func serverSide(t *testing.T) (fleet, member, string, *recorder, string) {
	t.Helper()
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	rec := &recorder{}
	addr := startServer(t, dir, srvM.keyPath, rec)
	return f, srvM, dir, rec, addr
}

func mustRefuse(t *testing.T, c *http.Client, addr string, rec *recorder, want error) {
	t.Helper()
	if body, err := get(c, addr); err == nil {
		t.Fatalf("request succeeded (%q); want refusal with %v", body, want)
	}
	// The handshake fails on the client side before the server's log line
	// is guaranteed visible; give the server goroutine a moment.
	deadline := time.Now().Add(2 * time.Second)
	for !rec.has(want.Error()) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !rec.has(want.Error()) {
		t.Fatalf("server log does not name %q:\n%s", want, rec)
	}
}

func TestServerConfig_ValidClientReachesHandlerAsItsFingerprint(t *testing.T) {
	f, _, _, rec, addr := serverSide(t)
	peer := f.enroll(t, "peer")
	body, err := get(f.clientFor(t, peer, nil), addr)
	if err != nil {
		t.Fatalf("%v\nlog:\n%s", err, rec)
	}
	if body != Fingerprint(peer.pub) {
		t.Fatalf("handler saw %q, want %q", body, Fingerprint(peer.pub))
	}
}

func TestServerConfig_SelfSignedClientRefusedWithReason(t *testing.T) {
	f, _, _, rec, addr := serverSide(t)
	keyPath, pub := writeOpenSSHKey(t)
	signer, _, _ := LoadSigner(keyPath)
	u, _ := parseURL(SAN(RoleInstance, "rogue", Fingerprint(pub)))
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(9), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: u}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, signer)
	c, _ := x509.ParseCertificate(der)
	mustRefuse(t, f.clientFor(t, member{keyPath: keyPath, pub: pub, cert: c}, nil), addr, rec, ErrUntrustedRoot)
}

func TestServerConfig_ExpiredClientRefusedWithReason(t *testing.T) {
	f, _, _, rec, addr := serverSide(t)
	old := f.enrollWindow(t, "old", time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour))
	mustRefuse(t, f.clientFor(t, old, nil), addr, rec, ErrExpired)
}

func TestServerConfig_OtherRootRefusedWithReason(t *testing.T) {
	f, _, _, rec, addr := serverSide(t)
	other := newFleet(t)
	stranger := other.enroll(t, "stranger")
	c := f.clientFor(t, stranger, nil) // verifies the server against f: fine
	mustRefuse(t, c, addr, rec, ErrUntrustedRoot)
}

// The reload test: the same running server refuses a serial the moment the
// published CRL lists it, and a session ticket from before does not help.
func TestServerConfig_RevocationTakesEffectWithoutRestartIncludingResumption(t *testing.T) {
	f, _, dir, rec, addr := serverSide(t)
	peer := f.enroll(t, "peer")
	cache := tls.NewLRUClientSessionCache(8)
	c := f.clientFor(t, peer, cache)
	var resumed []bool
	tr := c.Transport.(*http.Transport)
	inner := tr.TLSClientConfig.VerifyConnection
	tr.TLSClientConfig.VerifyConnection = func(cs tls.ConnectionState) error {
		resumed = append(resumed, cs.DidResume)
		return inner(cs)
	}
	for i := range 2 { // the second one resumes from the ticket
		if _, err := get(c, addr); err != nil {
			t.Fatalf("before revocation, request %d: %v\n%s", i, err, rec)
		}
	}
	// Falsifiability: the resumption half of this test means nothing unless
	// a resumption actually happened.
	if len(resumed) != 2 || resumed[0] || !resumed[1] {
		t.Fatalf("DidResume per request = %v, want [false true]", resumed)
	}
	if _, err := Revoke(f.dir, f.root, Revoked{Serial: peer.cert.SerialNumber, At: time.Now()}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.publishCRL(t, dir)
	// Client-side verification of the server uses f.crl, which does not list
	// the server; only the server's view changed.
	mustRefuse(t, c, addr, rec, ErrRevoked)
	if !rec.has("resumed true") {
		t.Fatalf("the post-revocation refusal was not on a RESUMED handshake:\n%s", rec)
	}
	if !rec.has("tls_reloaded") {
		t.Fatalf("no reload logged:\n%s", rec)
	}
	num, _ := ReadAcceptedNumber(dir)
	if num == nil || num.Cmp(big.NewInt(2)) != 0 {
		t.Fatalf("persisted crl.number = %v, want 2", num)
	}
	// A fresh client (no ticket) is refused as well.
	mustRefuse(t, f.clientFor(t, peer, nil), addr, rec, ErrRevoked)
}

func TestServerConfig_LowerCRLNumberNotAdoptedPreviousStillEnforced(t *testing.T) {
	f, _, dir, rec, addr := serverSide(t)
	peer := f.enroll(t, "peer")
	if _, err := Revoke(f.dir, f.root, Revoked{Serial: peer.cert.SerialNumber, At: time.Now()}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.publishCRL(t, dir) // Number 2, lists peer
	mustRefuse(t, f.clientFor(t, peer, nil), addr, rec, ErrRevoked)

	// Roll back: a validly signed CRL with Number 1 that lists nothing.
	if _, err := IssueCRL(f.dir, f.root, nil, big.NewInt(1), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.publishCRL(t, dir)
	rec2 := len(rec.String())
	mustRefuse(t, f.clientFor(t, peer, nil), addr, rec, ErrRevoked) // still refused: old list in force
	if !strings.Contains(rec.String()[rec2:], ErrCRLInvalid.Error()) || !strings.Contains(rec.String()[rec2:], "rollback") {
		t.Fatalf("rollback not logged as ErrCRLInvalid:\n%s", rec)
	}
	// Several more handshakes against the same rejected file: logged ONCE.
	for range 3 {
		get(f.clientFor(t, peer, nil), addr)
	}
	if n := rec.count("rollback"); n != 1 {
		t.Fatalf("the rejected CRL was logged %d times, want once:\n%s", n, rec)
	}
}

func TestServerConfig_RestartRemembersTheCRLNumber(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	if _, err := Revoke(f.dir, f.root, Revoked{Serial: big.NewInt(77), At: time.Now()}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.publishCRL(t, dir)
	if _, err := ServerConfig(dir, srvM.keyPath, nil); err != nil { // first boot adopts Number 2
		t.Fatal(err)
	}
	if _, err := IssueCRL(f.dir, f.root, nil, big.NewInt(1), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	f.publishCRL(t, dir) // an older CRL put in place while stopped
	if _, err := ServerConfig(dir, srvM.keyPath, nil); !errors.Is(err, ErrCRLInvalid) {
		t.Fatalf("restart with an older CRL: err=%v, want ErrCRLInvalid", err)
	}
}

func TestServerConfig_MissingCRLAtStartupFailsClosed(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	dir := f.install(t, srvM)
	if _, err := ServerConfig(dir, srvM.keyPath, nil); err != nil { // positive control
		t.Fatal(err)
	}
	os.Remove(filepath.Join(dir, CRLFile))
	if _, err := ServerConfig(dir, srvM.keyPath, nil); !errors.Is(err, ErrCRLMissing) {
		t.Fatalf("err=%v, want ErrCRLMissing", err)
	}
}

func TestServerConfig_CertificateForAnotherKeyRefusedAtStartup(t *testing.T) {
	f := newFleet(t)
	srvM := f.enroll(t, "server")
	other := f.enroll(t, "other")
	dir := f.install(t, srvM)
	if _, err := ServerConfig(dir, other.keyPath, nil); err == nil {
		t.Fatal("ServerConfig accepted a certificate that is not for its key")
	}
}

func TestServerConfig_StaleCRLAdoptedWarnedAndEnforced(t *testing.T) {
	f, _, dir, rec, addr := serverSide(t)
	peer := f.enroll(t, "peer")
	stale := staleCRLNumbered(t, f.root, big.NewInt(5), peer.cert.SerialNumber)
	if err := writeFileAtomic(filepath.Join(dir, CRLFile), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	mustRefuse(t, f.clientFor(t, peer, nil), addr, rec, ErrRevoked)
	if !rec.has("tls_crl_stale") {
		t.Fatalf("stale CRL not warned:\n%s", rec)
	}
	// Positive control: a peer the stale list does not name still gets in.
	other := f.enroll(t, "other")
	if _, err := get(f.clientFor(t, other, nil), addr); err != nil {
		t.Fatalf("unlisted peer refused under a stale CRL: %v\n%s", err, rec)
	}
}

func staleCRLNumbered(t *testing.T, root Root, number *big.Int, serials ...*big.Int) []byte {
	t.Helper()
	var entries []x509.RevocationListEntry
	for _, s := range serials {
		entries = append(entries, x509.RevocationListEntry{SerialNumber: s, RevocationTime: time.Now().Add(-48 * time.Hour)})
	}
	der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{
		Number: number, ThisUpdate: time.Now().Add(-48 * time.Hour), NextUpdate: time.Now().Add(-24 * time.Hour),
		RevokedCertificateEntries: entries,
	}, root.Cert, root.Signer)
	if err != nil {
		t.Fatal(err)
	}
	return crlPEM(der)
}

func parseURL(s string) ([]*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	return []*url.URL{u}, nil
}

// discardLogger swallows net/http's own "TLS handshake error" lines; the
// assertions read the recorder, which carries the named reason.
func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func (r *recorder) count(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// serverSerial connects and returns the serial of the certificate the
// server presented, i.e. which file set it is serving.
func serverSerial(t *testing.T, c *http.Client, addr string) (*big.Int, error) {
	t.Helper()
	resp, err := c.Get("https://" + addr + "/")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return resp.TLS.PeerCertificates[0].SerialNumber, nil
}

// install writes instance.crt, root.crt and crl.pem one rename at a time, so
// a handshake can land between them. A new instance.crt beside the OLD
// root.crt must not be served; the previous set stays whole.
func TestServerConfig_TornInstallKeepsThePreviousSetAndLogsOnce(t *testing.T) {
	f, srvM, dir, rec, addr := serverSide(t)
	peer := f.enroll(t, "peer")
	c := f.clientFor(t, peer, nil)
	before, err := serverSerial(t, c, addr)
	if err != nil || before.Cmp(srvM.cert.SerialNumber) != 0 {
		t.Fatalf("baseline: serial %v err %v", before, err)
	}

	// Re-enroll the SAME server key under a different root; write ONLY
	// instance.crt (the first of install's three renames).
	g := newFleet(t)
	newPEM, _, err := IssueInstance(g.dir, g.root, srvM.pub, "server", RoleInstance, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(filepath.Join(dir, InstanceCertFile), newPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		got, err := serverSerial(t, c, addr)
		if err != nil {
			t.Fatalf("handshake %d during the torn install: %v\n%s", i, err, rec)
		}
		if got.Cmp(before) != 0 {
			t.Fatalf("handshake %d served serial %v; the torn set was adopted", i, got)
		}
	}
	if n := rec.count("tls_reload_rejected"); n != 1 {
		t.Fatalf("torn set logged %d times over 3 handshakes, want once:\n%s", n, rec)
	}

	// Finish the install: root.crt and crl.pem of the new fleet. The set now
	// verifies together and is adopted on the next handshake.
	rootPEM, _ := os.ReadFile(filepath.Join(g.dir, RootCertFile))
	writeFileAtomic(filepath.Join(dir, RootCertFile), rootPEM, 0o600)
	g.publishCRL(t, dir)
	newPeer := g.enroll(t, "peer2")
	got, err := serverSerial(t, g.clientFor(t, newPeer, nil), addr)
	if err != nil {
		t.Fatalf("after the install completed: %v\n%s", err, rec)
	}
	if got.Cmp(before) == 0 {
		t.Fatal("the completed install was not adopted")
	}
	if _, err := get(c, addr); err == nil { // the old fleet's peer is now a stranger
		t.Fatal("a peer of the replaced root still gets in")
	}
}
