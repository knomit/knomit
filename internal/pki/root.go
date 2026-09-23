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
)

// Root is the fleet's trust anchor: the master certificate and, on the
// operator's machine only, its signer. Instances hold the certificate alone
// (LoadRootCert); the private half never lives on a fleet machine.
type Root struct {
	Cert   *x509.Certificate
	Signer crypto.Signer
}

// clockSkew backdates NotBefore so a peer whose clock runs a little behind
// the master's does not refuse a certificate issued moments ago. A defensive
// bound, not a measured property of any fleet.
const clockSkew = 5 * time.Minute

// NewRoot creates the fleet root in dir: a self-signed Ed25519 CA whose key
// is written ONLY passphrase-encrypted (OpenSSH format, 0600) and whose
// certificate is written beside it. It refuses to run over an existing root
// — replacing a root orphans every certificate in the fleet — and refuses an
// empty passphrase, since this key signs every identity.
func NewRoot(dir, cn string, passphrase []byte, validity time.Duration) (Root, error) {
	if len(passphrase) == 0 {
		return Root{}, errors.New("pki: the root key requires a passphrase")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Root{}, fmt.Errorf("pki: create master dir: %w", err)
	}
	keyPath := filepath.Join(dir, RootKeyFile)
	if _, err := os.Stat(keyPath); err == nil {
		return Root{}, fmt.Errorf("pki: %s already exists; refusing to replace the fleet root", keyPath)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Root{}, fmt.Errorf("pki: generate root key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return Root{}, err
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
		return Root{}, fmt.Errorf("pki: self-sign root: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return Root{}, err
	}

	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, cn, passphrase)
	if err != nil {
		return Root{}, fmt.Errorf("pki: encrypt root key: %w", err)
	}
	// O_EXCL: the Stat above is advisory; this is the check that holds under
	// a race with a second init-master.
	f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Root{}, fmt.Errorf("pki: write root key: %w", err)
	}
	if _, err := f.Write(pem.EncodeToMemory(block)); err != nil {
		f.Close()
		os.Remove(keyPath)
		return Root{}, fmt.Errorf("pki: write root key: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(keyPath)
		return Root{}, fmt.Errorf("pki: write root key: %w", err)
	}
	if err := writeFileAtomic(filepath.Join(dir, RootCertFile), certPEM(der), 0o644); err != nil {
		return Root{}, err
	}
	return Root{Cert: cert, Signer: priv}, nil
}

// LoadRoot reads the root key (decrypting it with passphrase) and its
// certificate, and refuses a pair whose halves do not match.
func LoadRoot(dir string, passphrase []byte) (Root, error) {
	raw, err := os.ReadFile(filepath.Join(dir, RootKeyFile))
	if err != nil {
		return Root{}, fmt.Errorf("pki: read root key: %w", err)
	}
	k, err := ssh.ParseRawPrivateKeyWithPassphrase(raw, passphrase)
	if err != nil {
		return Root{}, fmt.Errorf("pki: decrypt root key: %w", err)
	}
	var priv ed25519.PrivateKey
	switch v := k.(type) {
	case *ed25519.PrivateKey:
		priv = *v
	case ed25519.PrivateKey:
		priv = v
	default:
		return Root{}, fmt.Errorf("pki: root key is %T, want ed25519", k)
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
