package pki

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

// writeOpenSSHKey writes a key exactly the way internal/app/identity.go
// ensureKeyPair does (ssh.MarshalPrivateKey, no passphrase, 0600).
func writeOpenSSHKey(t *testing.T) (string, ed25519.PublicKey) {
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

func TestLoadSigner_RoundTripsTheSameKey(t *testing.T) {
	path, pub := writeOpenSSHKey(t)
	signer, gotPub, err := LoadSigner(path)
	if err != nil {
		t.Fatal(err)
	}
	if !gotPub.Equal(pub) {
		t.Fatal("public key differs from the one written")
	}
	msg := []byte("knomit")
	sig, err := signer.Sign(rand.Reader, msg, crypto.Hash(0)) // ed25519 signs the message, unhashed
	if err != nil || !ed25519.Verify(pub, msg, sig) {
		t.Fatalf("signature by loaded signer does not verify: %v", err)
	}
}

func TestLoadSigner_RefusesNonEd25519(t *testing.T) {
	// An RSA-shaped mistake would otherwise surface as a confusing TLS error
	// far from its cause.
	p := filepath.Join(t.TempDir(), "id_bad")
	if err := os.WriteFile(p, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSigner(p); err == nil {
		t.Fatal("LoadSigner accepted a non-key file")
	}
}

// The branch name in internal/app/identity.go is the first 8 hex of sha256
// over the SSH WIRE encoding. This pins that Fingerprint agrees with that and
// DISAGREES with the two plausible mistakes, so nobody can swap them.
func TestFingerprint_IsSSHWireNotSPKIOrRaw(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	wire := sha256.Sum256(sshPub.Marshal())
	want := hex.EncodeToString(wire[:])
	got := Fingerprint(pub)
	if got != want {
		t.Fatalf("Fingerprint = %s, want ssh-wire %s", got, want)
	}
	if len(got) != 64 {
		t.Fatalf("Fingerprint length %d, want 64", len(got))
	}
	raw := sha256.Sum256(pub)
	if got == hex.EncodeToString(raw[:]) {
		t.Fatal("Fingerprint must not equal sha256(raw key)")
	}
	spkiDER, _ := x509.MarshalPKIXPublicKey(pub)
	spki := sha256.Sum256(spkiDER)
	if got == hex.EncodeToString(spki[:]) {
		t.Fatal("Fingerprint must not equal sha256(SPKI DER)")
	}
	if Short(want) != want[:8] {
		t.Fatalf("Short = %q", Short(want))
	}
}

// Reproduces internal/app/identity.go fingerprint() inline (internal/app
// cannot be imported from here, and importing internal/pki from internal/app
// only for a test would add the dependency the wrong way round). If this ever
// diverges from identity.go:89-92, the branch name and the certificate SAN
// stop agreeing and every enrolled instance is refused with ErrSANMismatch.
func TestShort_EqualsTheBranchNameFingerprint(t *testing.T) {
	path, pub := writeOpenSSHKey(t)
	data, _ := os.ReadFile(path)
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(signer.PublicKey().Marshal()) // identity.go:90
	branchFP := hex.EncodeToString(h[:])[:8]         // identity.go:91
	if got := Short(Fingerprint(pub)); got != branchFP {
		t.Fatalf("Short(Fingerprint) = %s, branch name fingerprint = %s", got, branchFP)
	}
}

func TestPKCS8_ParsesBackToSameKey(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, err := PKCS8(priv)
	if err != nil {
		t.Fatal(err)
	}
	back, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatal(err)
	}
	if !back.(ed25519.PrivateKey).Public().(ed25519.PublicKey).Equal(pub) {
		t.Fatal("PKCS8 round trip changed the key")
	}
}
