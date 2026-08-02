package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// The signature must cover the SHA-256 DIGEST of the artifact, because that is
// what pkg/updater verifies (verify.go: ed25519.Verify(pub, digest, sig), with
// digest computed while streaming the download). Signing the file bytes instead
// — which is what Sparkle's own sign_update does — yields a feed that parses
// fine and fails every install. This test is the contract.
func TestSignFileCoversDigestNotContents(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("pretend this is a 40MB app bundle")
	path := filepath.Join(t.TempDir(), "Knomit-0.5.1-darwin-arm64.app.zip")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	sig64, err := SignFile(priv, path)
	if err != nil {
		t.Fatalf("SignFile: %v", err)
	}
	sig, err := base64.StdEncoding.DecodeString(sig64)
	if err != nil {
		t.Fatalf("signature is not valid base64: %v", err)
	}

	sum := sha256.Sum256(payload)
	if !ed25519.Verify(pub, sum[:], sig) {
		t.Error("signature does not verify over the SHA-256 digest")
	}
	if ed25519.Verify(pub, payload, sig) {
		t.Error("signature verifies over the file CONTENTS — wrong message, updater will reject it")
	}
}

func TestFileDigestMatchesSHA256(t *testing.T) {
	payload := []byte("knomit")
	path := filepath.Join(t.TempDir(), "artifact.tar.gz")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := FileDigest(path)
	if err != nil {
		t.Fatalf("FileDigest: %v", err)
	}
	want := sha256.Sum256(payload)
	if string(got) != string(want[:]) {
		t.Errorf("FileDigest = %x, want %x", got, want)
	}
}

// The release workflow signs with a glob over the whole staging directory, so
// a re-run passes the sidecars from the previous run back in. Signing those
// would litter the release with <artifact>.ed25519.ed25519.
func TestRunSignSkipsExistingSidecars(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("UPDATE_PRIVATE_KEY", base64.StdEncoding.EncodeToString(priv))

	dir := t.TempDir()
	artifact := filepath.Join(dir, "Knomit-0.5.1-darwin-arm64.app.zip")
	if err := os.WriteFile(artifact, []byte("bundle"), 0o644); err != nil {
		t.Fatal(err)
	}

	// First run signs the artifact.
	if err := runSign([]string{artifact}); err != nil {
		t.Fatalf("first runSign: %v", err)
	}
	// Second run gets the sidecar back too, as a glob would produce.
	if err := runSign([]string{artifact, artifact + sigSuffix}); err != nil {
		t.Fatalf("second runSign: %v", err)
	}

	if _, err := os.Stat(artifact + sigSuffix + sigSuffix); !os.IsNotExist(err) {
		t.Error("signed a sidecar: double-suffixed file exists")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("dir holds %v, want just the artifact and one sidecar", names)
	}
}

func TestRunSignFailsWhenEveryArgIsASidecar(t *testing.T) {
	// Silently succeeding here would let a workflow report a signed release
	// having signed nothing at all.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("UPDATE_PRIVATE_KEY", base64.StdEncoding.EncodeToString(priv))

	sidecar := filepath.Join(t.TempDir(), "a.zip"+sigSuffix)
	if err := os.WriteFile(sidecar, []byte("sig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runSign([]string{sidecar}); err == nil {
		t.Error("runSign succeeded having signed nothing, want an error")
	}
}

func TestPrivateKeyFromBase64RejectsWrongLength(t *testing.T) {
	// A truncated or public-key-length value must fail loudly at setup rather
	// than produce signatures nothing can verify.
	if _, err := PrivateKeyFromBase64(base64.StdEncoding.EncodeToString(make([]byte, 32))); err == nil {
		t.Error("PrivateKeyFromBase64 accepted a 32-byte key, want error")
	}
	if _, err := PrivateKeyFromBase64("not base64 at all!"); err == nil {
		t.Error("PrivateKeyFromBase64 accepted non-base64 input, want error")
	}
	if _, err := PrivateKeyFromBase64(""); err == nil {
		t.Error("PrivateKeyFromBase64 accepted an empty key, want error")
	}
}
