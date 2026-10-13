package pki

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/ssh"
)

// Root custody (fleet design D9). In daily use the root key lives
// UNENCRYPTED in the OS keychain (macOS Keychain, Windows Credential
// Manager, Linux Secret Service) under one item:
//
//	service KeychainService ("knomit"), account "fleet-root/<RootID>"
//
// RootID is the fingerprint of the root's Ed25519 key — the same 64-hex
// value the CRL watermark is filed under and `identity install --root`
// takes — so the item name is stable for the life of the root and two roots
// can never share an item. Where no keychain is usable (headless Linux with
// no Secret Service, a locked macOS keychain over SSH) the key falls back to
// the passphrase-encrypted root.key file in the master directory.
//
// The root CERTIFICATE (public) always stays in <dir>/root.crt: it is what
// OpenRoot reads first, to learn which keychain item to open.
//
// Backup and takeover move the key as the passphrase-encrypted file
// (ExportRoot / ImportRoot), from the command line only: there is no HTTP
// route and no UI for either.

// KeychainService is the keychain service every knomit root item is filed
// under.
const KeychainService = "knomit"

// KeychainAccount is the keychain account of the root whose RootID is
// rootID.
func KeychainAccount(rootID string) string { return "fleet-root/" + rootID }

// ErrKeychainItemNotFound is a Keychain's "no such item": the normal case
// for a root held in the file, distinct from a keychain that is unusable.
var ErrKeychainItemNotFound = errors.New("pki: keychain item not found")

// ErrNoRootKey: neither the keychain nor the master directory holds the
// key of the root in <dir>/root.crt (exported elsewhere, or never imported
// here).
var ErrNoRootKey = errors.New("pki: the fleet root key is not on this machine")

// Keychain is the OS secret store, narrowed to what custody needs. The
// production value is OSKeychain; tests inject a fake so they never touch
// the real keychain.
type Keychain interface {
	Get(service, account string) (string, error)
	Set(service, account, secret string) error
	Delete(service, account string) error
}

// OSKeychain is the platform keychain through github.com/zalando/go-keyring.
func OSKeychain() Keychain { return osKeychain{} }

type osKeychain struct{}

func (osKeychain) Get(service, account string) (string, error) {
	v, err := keyring.Get(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrKeychainItemNotFound
	}
	return v, err
}

func (osKeychain) Set(service, account, secret string) error {
	return keyring.Set(service, account, secret)
}

func (osKeychain) Delete(service, account string) error {
	err := keyring.Delete(service, account)
	if errors.Is(err, keyring.ErrNotFound) {
		return ErrKeychainItemNotFound
	}
	return err
}

// RootSource says where a root key was read from or stored.
type RootSource string

const (
	RootFromKeychain RootSource = "keychain"
	RootFromFile     RootSource = "file"
)

// Passphrase supplies the root passphrase when (and only when) it is
// needed: the keychain path never asks for it.
type Passphrase func() ([]byte, error)

// OpenRoot opens the fleet root whose certificate is <dir>/root.crt: from
// the keychain item for its RootID when that item exists, else from the
// passphrase-encrypted <dir>/root.key, and says which. A keychain that
// errors in any way other than "not found" (unavailable, locked) also falls
// back to the file. A keychain item that EXISTS but does not hold this
// root's key is an error, never a reason to try the file: a second copy
// that disagrees is something to look at, not to route around.
func OpenRoot(dir string, kc Keychain, pass Passphrase) (Root, RootSource, error) {
	cert, err := LoadRootCert(filepath.Join(dir, RootCertFile))
	if err != nil {
		return Root{}, "", fmt.Errorf("pki: no fleet root in %s (run `knomit identity init-master` or `import-root`): %w", dir, err)
	}
	id, err := RootID(cert)
	if err != nil {
		return Root{}, "", err
	}
	account := KeychainAccount(id)
	secret, kerr := kc.Get(KeychainService, account)
	if kerr == nil {
		priv, err := parseRootSecret([]byte(secret))
		if err != nil {
			return Root{}, "", fmt.Errorf("pki: keychain item %s/%s: %w", KeychainService, account, err)
		}
		if !priv.Public().(ed25519.PublicKey).Equal(cert.PublicKey) {
			return Root{}, "", fmt.Errorf("pki: keychain item %s/%s does not hold the key of %s", KeychainService, account, filepath.Join(dir, RootCertFile))
		}
		return Root{Cert: cert, Signer: priv}, RootFromKeychain, nil
	}
	keyPath := filepath.Join(dir, RootKeyFile)
	if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
		return Root{}, "", fmt.Errorf("%w: keychain item %s/%s: %v; file %s: absent", ErrNoRootKey, KeychainService, account, kerr, keyPath)
	}
	if pass == nil {
		return Root{}, "", fmt.Errorf("pki: the root key is in the passphrase-encrypted %s (keychain: %v), and no passphrase was given", keyPath, kerr)
	}
	p, err := pass()
	if err != nil {
		return Root{}, "", err
	}
	root, err := LoadRoot(dir, p)
	if err != nil {
		return Root{}, "", err
	}
	return root, RootFromFile, nil
}

// CreateRoot is `identity init-master`: it mints a new fleet root in dir and
// stores its key in the keychain, or — when the keychain refuses — in the
// passphrase-encrypted <dir>/root.key (pass is called only then). It refuses
// to run over an existing root.crt or root.key: replacing a root orphans
// every certificate in the fleet.
func CreateRoot(dir, cn string, validity time.Duration, kc Keychain, pass Passphrase) (Root, RootSource, error) {
	for _, f := range []string{RootCertFile, RootKeyFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
			return Root{}, "", fmt.Errorf("pki: %s already exists; refusing to replace the fleet root", filepath.Join(dir, f))
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Root{}, "", fmt.Errorf("pki: create master dir: %w", err)
	}
	root, priv, err := generateRoot(cn, validity)
	if err != nil {
		return Root{}, "", err
	}
	id, err := RootID(root.Cert)
	if err != nil {
		return Root{}, "", err
	}
	certPath := filepath.Join(dir, RootCertFile)
	// O_EXCL: the Stat above is advisory; this holds under a race with a
	// second init-master.
	if err := writeExclusive(certPath, certPEM(root.Cert.Raw), 0o644); err != nil {
		return Root{}, "", err
	}
	secret, err := rootSecret(priv, cn)
	if err != nil {
		os.Remove(certPath)
		return Root{}, "", err
	}
	kerr := kc.Set(KeychainService, KeychainAccount(id), string(secret))
	if kerr == nil {
		return root, RootFromKeychain, nil
	}
	if pass == nil {
		os.Remove(certPath)
		return Root{}, "", fmt.Errorf("pki: the OS keychain refused the root key (%v); give a passphrase to store it encrypted in %s instead", kerr, filepath.Join(dir, RootKeyFile))
	}
	p, err := pass()
	if err == nil {
		err = writeEncryptedRootKey(filepath.Join(dir, RootKeyFile), priv, cn, p)
	}
	if err != nil {
		os.Remove(certPath)
		return Root{}, "", fmt.Errorf("pki: the OS keychain refused the root key (%v), and the file fallback failed: %w", kerr, err)
	}
	return root, RootFromFile, nil
}

// ExportResult says what ExportRoot did.
type ExportResult struct {
	From            RootSource // where the key was read
	RemovedKeychain bool       // the keychain item was deleted
	RemovedFile     bool       // <dir>/root.key was deleted
}

// ExportRoot writes the root key, passphrase-encrypted exactly as root.key
// is (OpenSSH format), followed by the root certificate, to out — a new
// file, 0600, never overwritten. passphrase encrypts the export and, when
// the key is held in the file, also opens it.
//
// The export is a MOVE unless keep is set: after the written file has been
// read back and decrypted to the same key, the keychain item and
// <dir>/root.key are removed, so two masters never coexist by accident. The
// root certificate and the ledgers stay in dir.
func ExportRoot(dir, out string, kc Keychain, passphrase []byte, keep bool) (ExportResult, error) {
	if len(passphrase) == 0 {
		return ExportResult{}, errors.New("pki: the exported root key requires a passphrase")
	}
	root, src, err := OpenRoot(dir, kc, func() ([]byte, error) { return passphrase, nil })
	if err != nil {
		return ExportResult{}, err
	}
	res := ExportResult{From: src}
	priv := root.Signer.(ed25519.PrivateKey)
	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, root.Cert.Subject.CommonName, passphrase)
	if err != nil {
		return res, fmt.Errorf("pki: encrypt root key: %w", err)
	}
	data := append(pem.EncodeToMemory(block), certPEM(root.Cert.Raw)...)
	if err := writeExclusive(out, data, 0o600); err != nil {
		return res, err
	}
	// Read it back before anything is removed: a move must never lose the
	// only copy.
	back, _, err := readRootFile(out, passphrase)
	if err != nil || !back.Equal(priv) {
		os.Remove(out)
		return res, fmt.Errorf("pki: the export did not read back as the root key; nothing removed: %v", err)
	}
	if keep {
		return res, nil
	}
	id, _ := RootID(root.Cert)
	switch err := kc.Delete(KeychainService, KeychainAccount(id)); {
	case err == nil:
		res.RemovedKeychain = true
	case errors.Is(err, ErrKeychainItemNotFound):
	default:
		if src == RootFromKeychain {
			return res, fmt.Errorf("pki: exported to %s, but the keychain item could not be removed, so the key is now in two places: %w", out, err)
		}
	}
	keyPath := filepath.Join(dir, RootKeyFile)
	switch err := os.Remove(keyPath); {
	case err == nil:
		res.RemovedFile = true
	case errors.Is(err, os.ErrNotExist):
	default:
		return res, fmt.Errorf("pki: exported to %s, but %s could not be removed, so the key is now in two places: %w", out, keyPath, err)
	}
	return res, nil
}

// ImportRoot loads a file written by ExportRoot (or a root.key, with the
// root certificate already in dir) into the keychain, falling back to
// <dir>/root.key when the keychain refuses it. The root certificate is
// taken from the file, else from <dir>/root.crt; the two halves must be a
// pair, and a dir that already holds a DIFFERENT root is refused. root.crt
// is written to dir when missing.
func ImportRoot(dir, file string, kc Keychain, passphrase []byte) (Root, RootSource, error) {
	priv, fileCert, err := readRootFile(file, passphrase)
	if err != nil {
		return Root{}, "", err
	}
	certPath := filepath.Join(dir, RootCertFile)
	installed, ierr := LoadRootCert(certPath)
	if ierr != nil && !errors.Is(ierr, os.ErrNotExist) {
		return Root{}, "", ierr
	}
	cert := fileCert
	if cert == nil {
		if installed == nil {
			return Root{}, "", fmt.Errorf("pki: %s holds no root certificate and %s is absent", file, certPath)
		}
		cert = installed
	}
	if !priv.Public().(ed25519.PublicKey).Equal(cert.PublicKey) {
		return Root{}, "", fmt.Errorf("pki: the key in %s is not the key of its root certificate", file)
	}
	if installed != nil && !bytes.Equal(installed.Raw, cert.Raw) {
		return Root{}, "", fmt.Errorf("pki: %s already holds a different fleet root; refusing to import over it", dir)
	}
	id, err := RootID(cert)
	if err != nil {
		return Root{}, "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Root{}, "", fmt.Errorf("pki: create master dir: %w", err)
	}
	if installed == nil {
		if err := writeFileAtomic(certPath, certPEM(cert.Raw), 0o644); err != nil {
			return Root{}, "", err
		}
	}
	root := Root{Cert: cert, Signer: priv}
	secret, err := rootSecret(priv, cert.Subject.CommonName)
	if err != nil {
		return Root{}, "", err
	}
	kerr := kc.Set(KeychainService, KeychainAccount(id), string(secret))
	if kerr == nil {
		return root, RootFromKeychain, nil
	}
	if err := writeEncryptedRootKey(filepath.Join(dir, RootKeyFile), priv, cert.Subject.CommonName, passphrase); err != nil {
		return Root{}, "", fmt.Errorf("pki: the OS keychain refused the root key (%v), and the file fallback failed: %w", kerr, err)
	}
	return root, RootFromFile, nil
}

// readRootFile decrypts the first PEM block of path (an OpenSSH private key)
// and returns the root certificate that follows it, if any.
func readRootFile(path string, passphrase []byte) (ed25519.PrivateKey, *x509.Certificate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: read root key file: %w", err)
	}
	keyBlock, rest := pem.Decode(raw)
	if keyBlock == nil {
		return nil, nil, fmt.Errorf("pki: %s holds no PEM key", path)
	}
	k, err := ssh.ParseRawPrivateKeyWithPassphrase(pem.EncodeToMemory(keyBlock), passphrase)
	if err != nil {
		return nil, nil, fmt.Errorf("pki: decrypt %s: %w", path, err)
	}
	priv, err := asEd25519(k)
	if err != nil {
		return nil, nil, err
	}
	var cert *x509.Certificate
	for {
		var blk *pem.Block
		blk, rest = pem.Decode(rest)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" || cert != nil {
			return nil, nil, fmt.Errorf("pki: %s: unexpected %q block after the key", path, blk.Type)
		}
		if cert, err = parseOneCert(pem.EncodeToMemory(blk)); err != nil {
			return nil, nil, fmt.Errorf("pki: %s: %w", path, err)
		}
		if !cert.IsCA {
			return nil, nil, fmt.Errorf("pki: %s: the certificate is not a CA certificate", path)
		}
	}
	return priv, cert, nil
}

// rootSecret is the keychain form of the root key: the unencrypted OpenSSH
// PEM, about 400 bytes (well under the Windows credential cap).
func rootSecret(priv ed25519.PrivateKey, cn string) ([]byte, error) {
	block, err := ssh.MarshalPrivateKey(priv, cn)
	if err != nil {
		return nil, fmt.Errorf("pki: encode root key: %w", err)
	}
	return pem.EncodeToMemory(block), nil
}

func parseRootSecret(secret []byte) (ed25519.PrivateKey, error) {
	k, err := ssh.ParseRawPrivateKey(secret)
	if err != nil {
		return nil, fmt.Errorf("not an OpenSSH private key: %w", err)
	}
	return asEd25519(k)
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
