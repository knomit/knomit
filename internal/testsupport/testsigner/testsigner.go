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
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// binary (store.SetTestFallbackSigner), and FixtureAgentID the agent id of
// every store and repos.Manager given none (store.SetTestFallbackAgentID).
func Install() {
	store.SetTestFallbackSigner(Signer())
	store.SetTestFallbackAgentID(FixtureAgentID)
}

// FixtureAgentID is the agent id a test fixture that names only an agent
// branch authors as: the branch without its "agent/" prefix. It is a FIXTURE
// CONVENTION, so the many tests that build a store from a branch name alone
// keep matching their member records; production never derives an id from a
// branch (D18) and passes the persisted instance_identity.agent_id instead.
// A test that must prove the persisted id wins sets one that differs from
// this. Any '/' left (a fixture branch such as "machine/test") becomes '-',
// because an agent id may not hold '/' (pki.ValidAgentID).
func FixtureAgentID(branch string) string {
	return strings.ReplaceAll(strings.TrimPrefix(branch, "agent/"), "/", "-")
}

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

// WriteFallbackKey writes the fallback signer's private key (OpenSSH format,
// 0600) and its .pub next to it under dir, and returns the private key's path.
// A fixture that builds "this machine's" commits with stock git signs them
// with it (git -c gpg.format=ssh -c user.signingkey=<path> commit -S), so a
// signer-less store in the same test binary recognises them as its own.
func WriteFallbackKey(dir string) (string, error) {
	seed := sha256.Sum256([]byte("knomit test fallback signer"))
	priv := ed25519.NewKeyFromSeed(seed[:])
	block, err := ssh.MarshalPrivateKey(priv, "knomit test fallback")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "fallback_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(path+".pub", ssh.MarshalAuthorizedKey(Signer().PublicKey()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// WriteNamedKey writes Named(name)'s private key (OpenSSH format, 0600) under
// dir and returns its path: the instance key file of a fixture whose commits
// Named(name) signs, so a certificate issued for that file's key names the
// signing key (a fleet join checks its bundle against the key file).
func WriteNamedKey(dir, name string) (string, error) {
	seed := sha256.Sum256([]byte("knomit named test signer: " + name))
	block, err := ssh.MarshalPrivateKey(ed25519.NewKeyFromSeed(seed[:]), name)
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+"_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
