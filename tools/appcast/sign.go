package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
)

// sigSuffix is appended to an artifact's filename to name its detached
// signature. These sidecars ship as release assets so the feed can be
// regenerated from the GitHub releases API alone, with no branch state to
// drift or be lost.
const sigSuffix = ".ed25519"

// FileDigest returns the SHA-256 of path's contents. This is the message
// SignFile signs and the digest pkg/updater computes while streaming a
// download — the two must agree or nothing installs.
func FileDigest(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("hash %s: %w", path, err)
	}
	return h.Sum(nil), nil
}

// SignFile returns the base64 Ed25519 signature over path's SHA-256 digest,
// ready to drop into a feed entry's sparkle:edSignature attribute.
//
// It signs the DIGEST, not the file bytes: pkg/updater's ed25519 verifier
// calls ed25519.Verify(pub, digest, sig). Sparkle's own sign_update signs
// contents, so a signature produced by that tool will not verify here — and
// vice versa. The failure mode is silent: the feed parses, the download
// succeeds, and every install is rejected at the last step.
func SignFile(priv ed25519.PrivateKey, path string) (string, error) {
	digest, err := FileDigest(path)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ed25519.Sign(priv, digest)), nil
}

// PrivateKeyFromBase64 decodes a standard-base64 Ed25519 private key. A wrong
// length is rejected here rather than at signing time, so a misconfigured
// secret fails the release run instead of publishing signatures no client can
// verify.
func PrivateKeyFromBase64(s string) (ed25519.PrivateKey, error) {
	if s == "" {
		return nil, fmt.Errorf("private key is empty")
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode private key: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	return ed25519.PrivateKey(raw), nil
}
