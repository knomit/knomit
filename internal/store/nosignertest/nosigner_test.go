// Package nosignertest holds the one test binary that runs WITHOUT the test
// fallback signer (no TestMain installs it), so it sees exactly what a
// production knomit sees: a Service without SetSigner cannot commit.
package nosignertest

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"

	"knomit/internal/store"
)

func openRepo(t *testing.T) *store.Service {
	t.Helper()
	svc, err := store.Open(filepath.Join(t.TempDir(), "repo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	// Init commits are written by the plumbing layer, which never signs: a new
	// repository must still be creatable without a signer.
	if err := svc.InitRepo(map[string]string{"README.md": "seed"}, "agent/test-deadbeef"); err != nil {
		t.Fatalf("InitRepo without a signer must still work: %v", err)
	}
	return svc
}

const factBody = "---\ntype: observation\nconfidence: 0.9\ndomain: [test]\n---\n# t\n\nbody\n"

// TestNoSigner_AuthoredWritesAreRefused: every authored write path refuses
// with store.ErrNoSigner instead of committing unsigned (F09 PR 2).
func TestNoSigner_AuthoredWritesAreRefused(t *testing.T) {
	svc := openRepo(t)
	ctx := context.Background()
	_, err := svc.Facts().WriteFact(ctx, "agent/test-deadbeef", "kb/notes/a.md", factBody, "learn: a", "created")
	if !errors.Is(err, store.ErrNoSigner) {
		t.Fatalf("WriteFact without a signer: got %v, want ErrNoSigner", err)
	}
}

// TestNoSigner_WithASignerTheSameWriteCommits: the refusal is about the
// missing signer and nothing else.
func TestNoSigner_WithASignerTheSameWriteCommits(t *testing.T) {
	svc := openRepo(t)
	seed := sha256.Sum256([]byte("nosignertest"))
	signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		t.Fatal(err)
	}
	svc.SetSigner(signer)
	if _, err := svc.Facts().WriteFact(context.Background(), "agent/test-deadbeef", "kb/notes/a.md", factBody, "learn: a", "created"); err != nil {
		t.Fatalf("WriteFact with a signer: %v", err)
	}
}

// TestNoSigner_RefusalLeavesNoObjects: the signer is resolved before any blob,
// tree or commit is written, so a refused write leaves the object store as it
// was (no orphan objects for Verify to report).
func TestNoSigner_RefusalLeavesNoObjects(t *testing.T) {
	svc := openRepo(t)
	ctx := context.Background()
	for i, write := range []func() error{
		func() error {
			_, err := svc.Facts().WriteFact(ctx, "agent/test-deadbeef", "kb/notes/a.md", factBody, "learn: a", "created")
			return err
		},
		func() error {
			_, _, err := svc.Facts().BatchWriteFacts(ctx, "agent/test-deadbeef",
				map[string]string{"kb/notes/b.md": factBody}, nil, "learn: b", "created")
			return err
		},
	} {
		if err := write(); !errors.Is(err, store.ErrNoSigner) {
			t.Fatalf("write %d: got %v, want ErrNoSigner", i, err)
		}
	}
	rep, err := svc.Verify(ctx, store.VerifyOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.IsStrictlyClean() {
		t.Fatalf("a refused write must leave nothing behind: %v", rep.Issues)
	}
}

// TestSignedWrite_LeavesNoUnsignedPreImage: a signed commit is signed in
// memory and stored once. The old path stored the unsigned commit first and
// left it unreachable, one orphan object per write.
func TestSignedWrite_LeavesNoUnsignedPreImage(t *testing.T) {
	svc := openRepo(t)
	seed := sha256.Sum256([]byte("nosignertest"))
	signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(seed[:]))
	if err != nil {
		t.Fatal(err)
	}
	svc.SetSigner(signer)
	ctx := context.Background()
	if _, err := svc.Facts().WriteFact(ctx, "agent/test-deadbeef", "kb/notes/a.md", factBody, "learn: a", "created"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Facts().DeleteFact(ctx, "agent/test-deadbeef", "kb/notes/a.md", "retract: a"); err != nil {
		t.Fatal(err)
	}
	rep, err := svc.Verify(ctx, store.VerifyOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.IsStrictlyClean() {
		t.Fatalf("signed writes must leave no unreachable objects: %v", rep.Issues)
	}
}
