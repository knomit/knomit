package app

import (
	"crypto/ed25519"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"knomit/internal/pki"
)

// The agent branch name and the certificate SAN carry the same 8 hex, and the
// principal ID carries all 64 of the same hash. If ensureKeyPair's fingerprint
// ever stopped being the prefix of pki.Fingerprint, every enrolled instance
// would be refused with ErrSANMismatch while its branch looked fine.
func TestEnsureKeyPair_FingerprintIsPrefixOfPKIFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")
	signer, fp8, err := ensureKeyPair(path) // generates
	if err != nil {
		t.Fatal(err)
	}
	_, pub, err := pki.LoadSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := pki.Short(pki.Fingerprint(pub)); got != fp8 {
		t.Fatalf("branch fingerprint %s != pki.Short(pki.Fingerprint) %s", fp8, got)
	}
	// Same key both ways: the ssh.Signer and the crypto.Signer name one key.
	cpk, ok := signer.PublicKey().(ssh.CryptoPublicKey)
	if !ok || !pub.Equal(cpk.CryptoPublicKey().(ed25519.PublicKey)) {
		t.Fatal("pki.LoadSigner read a different key than ensureKeyPair returned")
	}
	// Reload path: the second call READS the key rather than generating.
	_, fp8Again, err := ensureKeyPair(path)
	if err != nil || fp8Again != fp8 {
		t.Fatalf("reload fingerprint %s (err %v), want %s", fp8Again, err, fp8)
	}
}
