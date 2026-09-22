// Package pkitest builds throwaway fleets for tests in OTHER packages
// (internal/web, cmd): a root, enrolled members, and the files `knomit
// identity install` would write. Everything is generated in-process in
// t.TempDir(); no key material is ever committed.
package pkitest

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"knomit/internal/pki"
)

// Passphrase is the root passphrase every test fleet uses.
var Passphrase = []byte("pkitest passphrase")

// Fleet is a root with a published CRL, in its own master dir.
type Fleet struct {
	Dir  string
	Root pki.Root
}

// Member is one enrolled key.
type Member struct {
	KeyPath string
	Pub     ed25519.PublicKey
	CertPEM []byte
	Cert    *x509.Certificate
}

// Fingerprint is the member's full fingerprint (its principal ID).
func (m Member) Fingerprint() string { return pki.Fingerprint(m.Pub) }

// New creates a fleet whose CRL (Number 1) lists nothing.
func New(t testing.TB) *Fleet {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "master")
	root, err := pki.NewRoot(dir, "knomit-master-test", Passphrase, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pki.IssueCRL(dir, root, nil, big.NewInt(1), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return &Fleet{Dir: dir, Root: root}
}

// NewKey writes an instance key exactly as ensureKeyPair does (OpenSSH PEM,
// no passphrase, 0600) and returns its path.
func NewKey(t testing.TB) (string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(p, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return p, pub
}

// Enroll issues a certificate for a fresh key (or for keyPath's key when
// given) with the given role.
func (f *Fleet) Enroll(t testing.TB, host string, role pki.Role, keyPath ...string) Member {
	t.Helper()
	var path string
	var pub ed25519.PublicKey
	if len(keyPath) > 0 {
		path = keyPath[0]
		_, p, err := pki.LoadSigner(path)
		if err != nil {
			t.Fatal(err)
		}
		pub = p
	} else {
		path, pub = NewKey(t)
	}
	certPEM, _, err := pki.IssueInstance(f.Dir, f.Root, pub, host, role, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(certPEM)
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return Member{KeyPath: path, Pub: pub, CertPEM: certPEM, Cert: c}
}

// Install writes instance.crt, root.crt and crl.pem for m into dir (created
// 0700), what `knomit identity install` leaves behind.
func (f *Fleet) Install(t testing.TB, m Member, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rootPEM, err := os.ReadFile(filepath.Join(f.Dir, pki.RootCertFile))
	if err != nil {
		t.Fatal(err)
	}
	crlPEM, err := os.ReadFile(filepath.Join(f.Dir, pki.CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string][]byte{pki.InstanceCertFile: m.CertPEM, pki.RootCertFile: rootPEM, pki.CRLFile: crlPEM} {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Revoke revokes m on the master and copies the new CRL into each dir.
func (f *Fleet) Revoke(t testing.TB, m Member, dirs ...string) {
	t.Helper()
	if _, err := pki.Revoke(f.Dir, f.Root, pki.Revoked{Serial: m.Cert.SerialNumber, At: time.Now()}, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(f.Dir, pki.CRLFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dirs {
		tmp := filepath.Join(d, "."+pki.CRLFile+".tmp")
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(d, pki.CRLFile)); err != nil {
			t.Fatal(err)
		}
	}
}

// Client is an HTTPS client presenting m's certificate that verifies the
// server against f's root and CURRENT master CRL with pki.VerifyInstanceChain
// — the same fail-closed shape as pki.ClientConfig. Keep-alives are off so
// every request is a fresh handshake.
func (f *Fleet) Client(t testing.TB, m Member) *http.Client {
	t.Helper()
	signer, _, err := pki.LoadSigner(m.KeyPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{
		MinVersion:         tls.VersionTLS13,
		Certificates:       []tls.Certificate{{Certificate: [][]byte{m.Cert.Raw}, PrivateKey: signer, Leaf: m.Cert}},
		InsecureSkipVerify: true, // ONLY because VerifyConnection below is the full verifier
		VerifyConnection: func(cs tls.ConnectionState) error {
			crl, err := pki.LoadCRL(filepath.Join(f.Dir, pki.CRLFile))
			if err != nil {
				return err
			}
			_, err = pki.VerifyInstanceChain(cs.PeerCertificates[0], cs.PeerCertificates[1:], f.Root.Cert, crl, time.Now(), pki.UsageServer)
			return err
		},
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}}
}
