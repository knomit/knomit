// Package pki is the pure certificate core for F19 phase 2: the instance key
// as a crypto.Signer, the fleet root, instance issuance, CRLs, chain
// verification, and the server and client tls.Config. It has no store, web or
// repos dependency; integration lives in cmd/, internal/auth and internal/web.
package pki

import (
	"crypto"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
)

// LoadSigner reads the instance's OpenSSH-format private key — the ONLY copy
// of it, ~/.knomit/id_ed25519 unless [remote].ssh_key says otherwise — into
// memory as a crypto.Signer. The key is never re-written in another format:
// TLS and X.509 get the in-memory value, so there is still exactly one file
// whose theft is the whole compromise.
//
// ssh.ParseRawPrivateKey returns *ed25519.PrivateKey (a POINTER) for this key
// type; it is dereferenced here so callers, and tls.Certificate, see the value
// type that crypto/ed25519 documents.
func LoadSigner(path string) (crypto.Signer, ed25519.PublicKey, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read key %s: %w", path, err)
	}
	k, err := ssh.ParseRawPrivateKey(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("parse key %s: %w", path, err)
	}
	var priv ed25519.PrivateKey
	switch v := k.(type) {
	case *ed25519.PrivateKey:
		priv = *v
	case ed25519.PrivateKey:
		priv = v
	default:
		return nil, nil, fmt.Errorf("key %s is %T, want ed25519", path, k)
	}
	return priv, priv.Public().(ed25519.PublicKey), nil
}

// Fingerprint is THE fingerprint of an instance key: hex(sha256(SSH wire
// encoding of pub)), 64 hex characters. It is the same computation
// internal/app/identity.go truncates to 8 for the agent branch name, so a
// certificate, a branch and a grants row all name one key the same way.
//
// sha256 over the X.509 SPKI DER, or over the raw 32 key bytes, is a DIFFERENT
// value and must never be used for anything knomit compares to a branch name
// or a principal: it would silently mismatch every peer.
func Fingerprint(pub ed25519.PublicKey) string {
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		// Only a wrong-length key reaches here. Every caller holds either a
		// key it generated or one x509 already parsed, and x509 refuses an
		// Ed25519 key that is not 32 bytes, so this is a programming error.
		panic("pki: ed25519 key rejected by ssh.NewPublicKey: " + err.Error())
	}
	sum := sha256.Sum256(sshPub.Marshal())
	return hex.EncodeToString(sum[:])
}

// Short is the 8-hex prefix the branch name and the certificate SAN carry. It
// is a display and cross-check value, NEVER an identity: 32 bits can be
// ground. Principal IDs and grants rows use the full Fingerprint.
func Short(fp string) string { return fp[:8] }

// PKCS8 is the in-memory DER form some APIs need. Callers must not write it
// to disk: that would be a second copy of the instance key.
func PKCS8(priv ed25519.PrivateKey) ([]byte, error) {
	return x509.MarshalPKCS8PrivateKey(priv)
}
