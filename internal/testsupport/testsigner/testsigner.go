// Package testsigner installs one fixed commit signer for a test binary, so
// fixtures that open a store without SetSigner can still commit now that a
// signer-less write is store.ErrNoSigner. Call Install from TestMain.
//
// The key is derived from a fixed seed: deterministic, and never a real
// instance's key. A test that must prove a code path wires the REAL signer
// asserts the signature's key against its own, which this one never matches.
package testsigner

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"os/exec"
	"strings"

	"golang.org/x/crypto/ssh"

	"knomit/internal/store"
)

// Signer returns the fixed fallback test signer.
func Signer() ssh.Signer {
	seed := sha256.Sum256([]byte("knomit test fallback signer"))
	s, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		panic(err)
	}
	return s
}

// Install makes Signer the fallback for every signer-less store in this test
// binary (store.SetTestFallbackSigner).
func Install() { store.SetTestFallbackSigner(Signer()) }

// Named returns a deterministic signer derived from name, distinct from the
// fallback: give it to Deps.Signer / SetSigner in a test that must prove a
// path signs with the key it was GIVEN.
func Named(name string) ssh.Signer {
	seed := sha256.Sum256([]byte("knomit named test signer: " + name))
	s, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		panic(err)
	}
	return s
}

// CommitSignerKey returns the public key embedded in the SSHSIG signature of
// commit rev in the git repository at dir, read with real git (independent of
// the store code under test). ok is false when the commit carries no SSHSIG.
func CommitSignerKey(dir, rev string) (key ssh.PublicKey, ok bool, err error) {
	raw, err := exec.Command("git", "-C", dir, "cat-file", "commit", rev).Output()
	if err != nil {
		return nil, false, fmt.Errorf("git cat-file %s: %w", rev, err)
	}
	header, _, _ := strings.Cut(string(raw), "\n\n")
	var armor strings.Builder
	in := false
	for _, l := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(l, "gpgsig "):
			in = true
			armor.WriteString(strings.TrimPrefix(l, "gpgsig ") + "\n")
		case in && strings.HasPrefix(l, " "):
			armor.WriteString(l[1:] + "\n")
		default:
			in = false
		}
	}
	if !strings.Contains(armor.String(), "BEGIN SSH SIGNATURE") {
		return nil, false, nil
	}
	var b64 strings.Builder
	for _, l := range strings.Split(armor.String(), "\n") {
		if l != "" && !strings.HasPrefix(l, "-----") {
			b64.WriteString(strings.TrimSpace(l))
		}
	}
	env, err := base64.StdEncoding.DecodeString(b64.String())
	if err != nil || len(env) < 14 || string(env[:6]) != "SSHSIG" {
		return nil, false, fmt.Errorf("malformed SSHSIG on %s", rev)
	}
	rest := env[10:] // magic + uint32 version
	n := binary.BigEndian.Uint32(rest)
	pub, err := ssh.ParsePublicKey(rest[4 : 4+n])
	if err != nil {
		return nil, false, fmt.Errorf("SSHSIG key on %s: %w", rev, err)
	}
	return pub, true, nil
}

// SameKey reports whether a and b are the same public key.
func SameKey(a, b ssh.PublicKey) bool {
	return a != nil && b != nil && bytes.Equal(a.Marshal(), b.Marshal())
}
