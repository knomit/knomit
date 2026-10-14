package pki

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"

	"knomit/internal/platform/privdir"
)

// Root is the fleet's trust anchor: the fleet root certificate and, on the
// master only (the machine whose fleet root directory holds root.key), its
// signer. Every other instance holds the certificate alone (LoadRootCert).
type Root struct {
	Cert   *x509.Certificate
	Signer crypto.Signer
}

// clockSkew backdates NotBefore so a peer whose clock runs a little behind
// the fleet root's does not refuse a certificate issued moments ago. A defensive
// bound, not a measured property of any fleet.
const clockSkew = 5 * time.Minute

// ErrNoRootKey: the fleet root directory holds no root.key, so this machine
// is not the one that signs for the fleet (it is not the master).
var ErrNoRootKey = errors.New("pki: the fleet root key is not on this machine")

// NewRoot creates the fleet root in dir: a self-signed Ed25519 CA whose key
// is written as a plain, unencrypted OpenSSH private key (root.key, 0600),
// like an SSH key, and whose certificate is written beside it (root.crt).
// dir is made private to the user first (privdir.Ensure: 0700 on unix, a
// protected owner-only DACL on Windows), so everything written inside it
// inherits owner-only access on every OS. There is no keychain and no
// passphrase: whoever can read dir can sign for the fleet, exactly as with
// ~/.ssh/id_ed25519.
//
// It refuses to run over an existing root.key or root.crt: replacing a root
// orphans every certificate in the fleet. root.key is created O_EXCL, which
// is the check that holds under a race with a second writer.
func NewRoot(dir, cn string, validity time.Duration) (Root, error) {
	for _, f := range []string{RootKeyFile, RootCertFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return Root{}, fmt.Errorf("pki: %s already exists; refusing to replace the fleet root", filepath.Join(dir, f))
		}
	}
	if err := privdir.Ensure(dir); err != nil {
		return Root{}, fmt.Errorf("pki: create fleet root dir: %w", err)
	}
	root, priv, err := generateRoot(cn, validity)
	if err != nil {
		return Root{}, err
	}
	block, err := ssh.MarshalPrivateKey(priv, cn)
	if err != nil {
		return Root{}, fmt.Errorf("pki: encode root key: %w", err)
	}
	keyPath := filepath.Join(dir, RootKeyFile)
	if err := writeExclusive(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		return Root{}, fmt.Errorf("pki: write root key: %w", err)
	}
	if err := writeExclusive(filepath.Join(dir, RootCertFile), certPEM(root.Cert.Raw), 0o644); err != nil {
		os.Remove(keyPath)
		return Root{}, err
	}
	return root, nil
}

// generateRoot mints a self-signed Ed25519 CA, in memory only.
func generateRoot(cn string, validity time.Duration) (Root, ed25519.PrivateKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Root{}, nil, fmt.Errorf("pki: generate root key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return Root{}, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             now.Add(-clockSkew),
		NotAfter:              now.Add(validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		MaxPathLenZero:        true, // instance certificates are leaves; nothing chains below them
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return Root{}, nil, fmt.Errorf("pki: self-sign root: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return Root{}, nil, err
	}
	return Root{Cert: cert, Signer: priv}, priv, nil
}

// LoadRoot opens the fleet root in dir: the plain root.key and root.crt
// beside it. It asks for nothing. A missing root.key is ErrNoRootKey (this
// machine is not the master); a pair whose halves do not match is refused.
func LoadRoot(dir string) (Root, error) {
	keyPath := filepath.Join(dir, RootKeyFile)
	raw, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		return Root{}, fmt.Errorf("%w: %s is absent", ErrNoRootKey, keyPath)
	}
	if err != nil {
		return Root{}, fmt.Errorf("pki: read root key: %w", err)
	}
	k, err := ssh.ParseRawPrivateKey(raw)
	if err != nil {
		return Root{}, fmt.Errorf("pki: %s: %w", keyPath, err)
	}
	priv, err := asEd25519(k)
	if err != nil {
		return Root{}, err
	}
	cert, err := LoadRootCert(filepath.Join(dir, RootCertFile))
	if err != nil {
		return Root{}, err
	}
	if !priv.Public().(ed25519.PublicKey).Equal(cert.PublicKey) {
		return Root{}, errors.New("pki: root.key and root.crt are not a pair")
	}
	return Root{Cert: cert, Signer: priv}, nil
}

func asEd25519(k any) (ed25519.PrivateKey, error) {
	switch v := k.(type) {
	case *ed25519.PrivateKey:
		return *v, nil
	case ed25519.PrivateKey:
		return v, nil
	}
	return nil, fmt.Errorf("pki: root key is %T, want ed25519", k)
}

// writeExclusive creates path with data, refusing an existing file.
func writeExclusive(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(path)
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(path)
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	return nil
}

// LoadRootCert reads the public half: what every instance holds.
func LoadRootCert(path string) (*x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("pki: read root certificate: %w", err)
	}
	c, err := parseOneCert(raw)
	if err != nil {
		return nil, fmt.Errorf("pki: %s: %w", path, err)
	}
	if !c.IsCA {
		return nil, fmt.Errorf("pki: %s is not a CA certificate", path)
	}
	return c, nil
}

func parseOneCert(raw []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(raw)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("no CERTIFICATE PEM block")
	}
	return x509.ParseCertificate(blk.Bytes)
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// randomSerial is 128 random bits, as RFC 5280 §4.1.2.2 permits (≤ 20
// octets, positive). Revocation references it, so it must never repeat.
func randomSerial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("pki: serial: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil // never zero
}

// writeFileAtomic writes via a temp file in the same directory and a rename,
// so a reader (the TLS reloader) never sees a half-written file.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("pki: write %s: %w", path, err)
	}
	return nil
}
