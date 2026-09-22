package pki

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// HasInstanceCert reports whether an instance certificate is installed in
// dir. The TLS listener stays off until one is.
func HasInstanceCert(dir string) bool {
	fi, err := os.Stat(filepath.Join(dir, InstanceCertFile))
	return err == nil && fi.Mode().IsRegular()
}

// Logf receives one line per refusal or reload event: reason is the error
// text (which carries one of the named errors), kv are key/value pairs.
type Logf func(reason string, kv ...any)

// snapshot is one consistent set of the three files. It is replaced as a
// WHOLE: a reload in which any part fails keeps the previous snapshot in
// force, so the listener never runs with, say, a new root and the old CRL.
type snapshot struct {
	rawCert, rawRoot, rawCRL []byte
	cert                     tls.Certificate
	root                     *x509.Certificate
	crl                      *x509.RevocationList
}

// reloader re-reads instance.crt, root.crt and crl.pem on every handshake
// and adopts a changed set, so `knomit identity install` and a new CRL take
// effect without a restart. It compares file BYTES rather than mtimes: the
// files are a few KB, and a byte comparison cannot be fooled by mtime
// granularity or an inode reused by an atomic rename.
type reloader struct {
	dir    string
	signer crypto.Signer
	pub    ed25519.PublicKey
	logf   Logf

	mu      sync.Mutex
	cur     *snapshot
	lastNum *big.Int // highest CRL Number accepted, persisted in crl.number
	// rejected is the last file set that failed to load, remembered so a
	// bad set (a rolled-back CRL, a half-finished install) is parsed and
	// logged ONCE rather than on every handshake until someone fixes it.
	rejected *[3][]byte
}

// ServerConfig builds the TLS listener's config from <dir>/instance.crt,
// <dir>/root.crt and <dir>/crl.pem, with the instance key at keyPath (read
// into memory; never re-written).
//
// It FAILS CLOSED at construction: a missing or malformed CRL, a certificate
// that does not match the key, or a CRL older than the persisted crl.number
// is an error, and no listener should be opened.
func ServerConfig(dir, keyPath string, logf Logf) (*tls.Config, error) {
	r, snap, err := newReloader(dir, keyPath, logf)
	if err != nil {
		return nil, err
	}
	base := r.configFor(snap)
	base.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		return r.configFor(r.refresh()), nil
	}
	return base, nil
}

// newReloader loads and adopts the initial file set, failing closed.
func newReloader(dir, keyPath string, logf Logf) (*reloader, *snapshot, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	signer, pub, err := LoadSigner(keyPath)
	if err != nil {
		return nil, nil, err
	}
	lastNum, err := ReadAcceptedNumber(dir)
	if err != nil {
		return nil, nil, err
	}
	r := &reloader{dir: dir, signer: signer, pub: pub, logf: logf, lastNum: lastNum}
	snap, err := r.load()
	if err != nil {
		return nil, nil, err
	}
	if err := r.adopt(snap); err != nil {
		return nil, nil, err
	}
	return r, snap, nil
}

// configFor is the per-handshake config for one snapshot.
func (r *reloader) configFor(s *snapshot) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{s.cert},
		// RequireAnyClientCert, with ClientCAs deliberately nil, is the
		// STRONGER choice here, not the weaker one. Under
		// RequireAndVerifyClientCert, crypto/tls verifies the chain against
		// ClientCAs itself before VerifyConnection runs
		// (handshake_server_tls13.go processCertsFromClient), so an untrusted
		// or expired certificate is refused with no reason we can log, and
		// the CRL, SAN and fingerprint checks would be a second verifier that
		// could drift from the first. Here VerifyInstanceChain is the ONE
		// verifier and every refusal carries a named error. Do not "fix"
		// this to RequireAndVerifyClientCert.
		ClientAuth: tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			return r.verify(s, cs, UsageClient)
		},
	}
}

// verify is the ONE verifier for both directions: usage says whether the
// peer is a client connecting to us or a server we connected to.
func (r *reloader) verify(s *snapshot, cs tls.ConnectionState, usage Usage) error {
	if len(cs.PeerCertificates) == 0 {
		err := fmt.Errorf("%w: peer presented no certificate", ErrUntrustedRoot)
		r.logf(err.Error(), "event", "tls_refused")
		return err
	}
	// Runs on resumed connections too (crypto/tls calls VerifyConnection
	// for every handshake), so a revoked peer cannot ride a session ticket
	// issued before the revocation.
	if _, err := VerifyInstanceChain(cs.PeerCertificates[0], cs.PeerCertificates[1:], s.root, s.crl, time.Now(), usage); err != nil {
		r.logf(err.Error(), "event", "tls_refused", "resumed", cs.DidResume)
		return err
	}
	return nil
}

// refresh returns the snapshot to use for this handshake, adopting a changed
// file set if it passes every check and keeping the current one otherwise.
func (r *reloader) refresh() *snapshot {
	r.mu.Lock()
	cur := r.cur
	r.mu.Unlock()

	rawCert, errC := os.ReadFile(filepath.Join(r.dir, InstanceCertFile))
	rawRoot, errR := os.ReadFile(filepath.Join(r.dir, RootCertFile))
	rawCRL, errL := os.ReadFile(filepath.Join(r.dir, CRLFile))
	if errC == nil && errR == nil && errL == nil &&
		bytes.Equal(rawCert, cur.rawCert) && bytes.Equal(rawRoot, cur.rawRoot) && bytes.Equal(rawCRL, cur.rawCRL) {
		return cur
	}
	set := [3][]byte{rawCert, rawRoot, rawCRL}
	r.mu.Lock()
	seen := r.rejected != nil && errC == nil && errR == nil && errL == nil &&
		bytes.Equal(set[0], r.rejected[0]) && bytes.Equal(set[1], r.rejected[1]) && bytes.Equal(set[2], r.rejected[2])
	r.mu.Unlock()
	if seen {
		return cur
	}
	next, err := r.parse(rawCert, errC, rawRoot, errR, rawCRL, errL)
	if err == nil {
		err = r.adopt(next)
	}
	if err != nil {
		r.mu.Lock()
		r.rejected = &set
		r.mu.Unlock()
		r.logf(err.Error(), "event", "tls_reload_rejected", "dir", r.dir)
		return cur
	}
	r.logf("tls files reloaded", "event", "tls_reloaded", "dir", r.dir, "crl_number", next.crl.Number.String())
	return next
}

func (r *reloader) load() (*snapshot, error) {
	rawCert, errC := os.ReadFile(filepath.Join(r.dir, InstanceCertFile))
	rawRoot, errR := os.ReadFile(filepath.Join(r.dir, RootCertFile))
	rawCRL, errL := os.ReadFile(filepath.Join(r.dir, CRLFile))
	return r.parse(rawCert, errC, rawRoot, errR, rawCRL, errL)
}

// parse validates one file set without adopting it.
func (r *reloader) parse(rawCert []byte, errC error, rawRoot []byte, errR error, rawCRL []byte, errL error) (*snapshot, error) {
	if errC != nil {
		return nil, fmt.Errorf("pki: instance certificate: %w", errC)
	}
	if errR != nil {
		return nil, fmt.Errorf("pki: root certificate: %w", errR)
	}
	if errors.Is(errL, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrCRLMissing, filepath.Join(r.dir, CRLFile))
	}
	if errL != nil {
		return nil, fmt.Errorf("%w: %v", ErrCRLInvalid, errL)
	}
	leaf, err := parseOneCert(rawCert)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: %w", InstanceCertFile, err)
	}
	if !ed25519.PublicKey(r.pub).Equal(leaf.PublicKey) {
		return nil, fmt.Errorf("pki: %s is not a certificate for this instance's key", InstanceCertFile)
	}
	root, err := parseOneCert(rawRoot)
	if err != nil || !root.IsCA {
		return nil, fmt.Errorf("pki: %s: not a CA certificate: %v", RootCertFile, err)
	}
	// The three files must verify TOGETHER. `install` writes them one rename
	// at a time, so a handshake can see a new instance.crt beside the old
	// root.crt; such a set is rejected whole and the previous one stays.
	pool := x509.NewCertPool()
	pool.AddCert(root)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, fmt.Errorf("pki: %s does not chain to %s: %w", InstanceCertFile, RootCertFile, err)
	}
	crl, err := parseCRL(rawCRL)
	if err != nil {
		return nil, err
	}
	return &snapshot{
		rawCert: rawCert, rawRoot: rawRoot, rawCRL: rawCRL,
		cert: tls.Certificate{Certificate: [][]byte{leaf.Raw}, PrivateKey: r.signer, Leaf: leaf},
		root: root, crl: crl,
	}, nil
}

// adopt checks the snapshot's CRL against its root and the highest Number
// ever accepted, persists a higher Number, and makes the snapshot current.
// A stale CRL is adopted and ENFORCED, with a warning: refusing it would
// shut out every peer because the operator has not published lately, and
// ignoring it would un-revoke everything.
func (r *reloader) adopt(s *snapshot) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	stale, err := CheckCRL(s.crl, s.root, r.lastNum)
	if err != nil {
		return err
	}
	if stale {
		r.logf("CRL is stale (NextUpdate passed); still enforcing it; publish a fresh one with `knomit identity revoke` or a reissue",
			"event", "tls_crl_stale", "next_update", s.crl.NextUpdate, "crl_number", s.crl.Number.String())
	}
	if r.lastNum == nil || s.crl.Number.Cmp(r.lastNum) > 0 {
		if err := WriteAcceptedNumber(r.dir, s.crl.Number); err != nil {
			// Enforce the newer list now regardless; only the restart
			// protection is lost, and the operator must hear about it.
			r.logf("could not persist the CRL Number; a restart could accept an older CRL: "+err.Error(),
				"event", "tls_crl_number_unpersisted")
		}
		r.lastNum = new(big.Int).Set(s.crl.Number)
	}
	r.cur = s
	return nil
}
