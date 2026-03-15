package git_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"knomit/internal/git"
	"golang.org/x/crypto/ssh"
)

func generateTestSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return signer
}

func TestSignCommit_ProducesArmoredSignature(t *testing.T) {
	signer := generateTestSigner(t)
	sig, err := git.SignCommit(signer, []byte("tree abc\nauthor test"))
	if err != nil {
		t.Fatalf("SignCommit: %v", err)
	}
	if !strings.HasPrefix(sig, "-----BEGIN SSH SIGNATURE-----\n") {
		t.Error("missing BEGIN header")
	}
	if !strings.HasSuffix(sig, "\n-----END SSH SIGNATURE-----") {
		t.Error("missing END header")
	}
}

func TestSignCommit_EnvelopeHasMagic(t *testing.T) {
	signer := generateTestSigner(t)
	sig, err := git.SignCommit(signer, []byte("tree def\nauthor test"))
	if err != nil {
		t.Fatalf("SignCommit: %v", err)
	}

	// Strip armor headers and decode base64.
	lines := strings.Split(sig, "\n")
	var b64 strings.Builder
	for _, line := range lines {
		if strings.HasPrefix(line, "-----") {
			continue
		}
		b64.WriteString(line)
	}
	raw, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if len(raw) < 6 {
		t.Fatal("envelope too short")
	}
	if string(raw[:6]) != "SSHSIG" {
		t.Errorf("magic = %q, want %q", string(raw[:6]), "SSHSIG")
	}
}

func TestSignCommit_DifferentPayloadsProduceDifferentSignatures(t *testing.T) {
	signer := generateTestSigner(t)
	sig1, err := git.SignCommit(signer, []byte("payload one"))
	if err != nil {
		t.Fatalf("SignCommit(1): %v", err)
	}
	sig2, err := git.SignCommit(signer, []byte("payload two"))
	if err != nil {
		t.Fatalf("SignCommit(2): %v", err)
	}
	if sig1 == sig2 {
		t.Error("expected different signatures for different payloads")
	}
}
