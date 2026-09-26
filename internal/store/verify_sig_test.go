package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"

	"knomit/internal/pki"
)

func namedSigner(t *testing.T, name string) ssh.Signer {
	t.Helper()
	seed := sha256.Sum256([]byte("verify_sig_test " + name))
	s, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func testCommit() *object.Commit {
	when := time.Unix(1790000000, 0).UTC()
	return &object.Commit{
		Author:    object.Signature{Name: "a", Email: "host-8ef0cd32+learn@agents.knomit.io", When: when},
		Committer: object.Signature{Name: "a", Email: "host-8ef0cd32@agents.knomit.io", When: when},
		Message:   "learn: x",
		TreeHash:  plumbing.NewHash("4b825dc642cb6eb9a060e54bf8d69288fbee4904"),
	}
}

func signed(t *testing.T, c *object.Commit, s ssh.Signer) *object.Commit {
	t.Helper()
	payload, err := commitPayload(c)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := signCommit(s, payload)
	if err != nil {
		t.Fatal(err)
	}
	c.PGPSignature = sig
	return c
}

func TestVerifyCommitSignature_Valid(t *testing.T) {
	s := namedSigner(t, "k")
	got, err := verifyCommitSignature(signed(t, testCommit(), s))
	if err != nil {
		t.Fatalf("a commit signed by signCommit must verify: %v", err)
	}
	edPub := s.PublicKey().(ssh.CryptoPublicKey).CryptoPublicKey().(ed25519.PublicKey)
	if got.Fingerprint != pki.Fingerprint(edPub) || len(got.Fingerprint) != 64 {
		t.Fatalf("fingerprint = %q, want the full pki.Fingerprint of the signing key", got.Fingerprint)
	}
}

func TestVerifyCommitSignature_Refusals(t *testing.T) {
	s := namedSigner(t, "k")

	// Unsigned.
	if _, err := verifyCommitSignature(testCommit()); !errors.Is(err, ErrUnsigned) {
		t.Errorf("unsigned: got %v", err)
	}
	// PGP (GitHub web-flow).
	pgp := testCommit()
	pgp.PGPSignature = "-----BEGIN PGP SIGNATURE-----\n\nwsFcBAABCAAQ\n-----END PGP SIGNATURE-----"
	if _, err := verifyCommitSignature(pgp); !errors.Is(err, ErrNotSSHSIG) {
		t.Errorf("pgp: got %v", err)
	}
	// Tampered payload: the signature covers the message.
	tampered := signed(t, testCommit(), s)
	tampered.Message = "learn: something else"
	if _, err := verifyCommitSignature(tampered); !errors.Is(err, ErrBadSignature) {
		t.Errorf("tampered: got %v", err)
	}
	// A signature copied from another commit.
	other := testCommit()
	other.Message = "other"
	other.PGPSignature = signed(t, testCommit(), s).PGPSignature
	if _, err := verifyCommitSignature(other); !errors.Is(err, ErrBadSignature) {
		t.Errorf("transplanted: got %v", err)
	}
	// Garbage armor.
	bad := testCommit()
	bad.PGPSignature = "-----BEGIN SSH SIGNATURE-----\n!!!\n-----END SSH SIGNATURE-----"
	if _, err := verifyCommitSignature(bad); !errors.Is(err, ErrMalformedSig) {
		t.Errorf("garbage: got %v", err)
	}
	// A different namespace (a valid SSHSIG for "file", not "git").
	if _, err := verifyCommitSignature(wrongNamespace(t, testCommit(), s)); !errors.Is(err, ErrWrongNamespace) {
		t.Errorf("namespace: got %v", err)
	}
}

// wrongNamespace signs c exactly like signCommit but with namespace "file".
func wrongNamespace(t *testing.T, c *object.Commit, s ssh.Signer) *object.Commit {
	t.Helper()
	payload, err := commitPayload(c)
	if err != nil {
		t.Fatal(err)
	}
	c.PGPSignature = signWithNamespace(t, s, payload, "file")
	return c
}

// signWithNamespace is signCommit with the namespace as a parameter, so a test
// can build a correctly-formed SSHSIG for a namespace other than "git".
func signWithNamespace(t *testing.T, signer ssh.Signer, payload []byte, namespace string) string {
	t.Helper()
	h := sha512.Sum512(payload)
	var signedData []byte
	signedData = append(signedData, sshsigMagic...)
	signedData = appendSSHString(signedData, []byte(namespace))
	signedData = appendSSHString(signedData, nil)
	signedData = appendSSHString(signedData, []byte(sshsigHashAlgo))
	signedData = appendSSHString(signedData, h[:])
	sig, err := signer.Sign(rand.Reader, signedData)
	if err != nil {
		t.Fatal(err)
	}
	var env []byte
	env = append(env, sshsigMagic...)
	env = append(env, 0, 0, 0, 1)
	env = appendSSHString(env, signer.PublicKey().Marshal())
	env = appendSSHString(env, []byte(namespace))
	env = appendSSHString(env, nil)
	env = appendSSHString(env, []byte(sshsigHashAlgo))
	env = appendSSHString(env, ssh.Marshal(sig))
	return armor(env)
}

// TestVerifyCommitSignature_StockGitSSHSignature: an operator signs hand
// commits with stock git (gpg.format=ssh, ssh-keygen -Y sign). Those must
// verify exactly like knomit's own, or the operator's enables are refused.
func TestVerifyCommitSignature_StockGitSSHSignature(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen not available")
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "op")
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(args[0], args[1:]...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	run("git", "init", "-q", "repo")
	repo := filepath.Join(dir, "repo")
	gitc := func(args ...string) string {
		return run(append([]string{"git", "-C", repo, "-c", "user.name=op", "-c", "user.email=op@example.com",
			"-c", "gpg.format=ssh", "-c", "user.signingkey=" + key + ".pub"}, args...)...)
	}
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitc("add", "f")
	gitc("commit", "-q", "-S", "-m", "operator commit")
	head := gitc("rev-parse", "HEAD")

	r, err := gogit.PlainOpen(repo)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.CommitObject(plumbing.NewHash(head))
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifyCommitSignature(c)
	if err != nil {
		t.Fatalf("a stock git SSH-signed commit must verify: %v", err)
	}
	pubLine, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	want, _, _, _, err := ssh.ParseAuthorizedKey(pubLine)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Key.Marshal(), want.Marshal()) {
		t.Fatal("verified key is not the operator's key")
	}
}

// TestVerifyCommitSignature_KnomitWritePath: a commit made by the store's own
// write path verifies, with the store signer's fingerprint.
func TestVerifyCommitSignature_KnomitWritePath(t *testing.T) {
	svc := newExperimentTestStore(t)
	s := namedSigner(t, "store")
	svc.SetSigner(s)
	res, err := svc.Facts().WriteFact(context.Background(), testAgentBranch, "kb/notes/a.md",
		"---\ntype: observation\n---\n# a\n\nb\n", "learn: a", "created")
	if err != nil {
		t.Fatal(err)
	}
	c, err := svc.rh.repo.CommitObject(plumbing.NewHash(res.CommitHash))
	if err != nil {
		t.Fatal(err)
	}
	got, err := verifyCommitSignature(c)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got.Key.Marshal(), s.PublicKey().Marshal()) {
		t.Fatal("the write path's commit is not signed by the store signer")
	}
}
