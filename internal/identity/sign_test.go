package identity_test

import (
	"testing"
	"time"

	"knomit/internal/identity"
	storegit "knomit/internal/store/git"

	_ "github.com/mattn/go-sqlite3"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func newTestStorer(t *testing.T) *storegit.Storer {
	t.Helper()
	s, err := storegit.NewMemoryStorer()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// createTestCommit creates a minimal unsigned commit in the storer and returns its hash.
func createTestCommit(t *testing.T, s *storegit.Storer) plumbing.Hash {
	t.Helper()

	// Create an empty tree.
	treeObj := s.NewEncodedObject()
	tree := &object.Tree{}
	if err := tree.Encode(treeObj); err != nil {
		t.Fatal(err)
	}
	treeHash, err := s.SetEncodedObject(treeObj)
	if err != nil {
		t.Fatal(err)
	}

	// Create a commit pointing at the tree.
	commit := &object.Commit{
		Author: object.Signature{
			Name:  "test",
			Email: "test@test",
			When:  time.Now(),
		},
		Committer: object.Signature{
			Name:  "test",
			Email: "test@test",
			When:  time.Now(),
		},
		Message:  "test commit",
		TreeHash: treeHash,
	}
	commitObj := s.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		t.Fatal(err)
	}
	commitHash, err := s.SetEncodedObject(commitObj)
	if err != nil {
		t.Fatal(err)
	}
	return commitHash
}

func TestSignCommitInPlace_SignsAndChangesHash(t *testing.T) {
	s := newTestStorer(t)
	signer := generateTestSigner(t)
	commitHash := createTestCommit(t, s)

	newHash, err := identity.SignCommitInPlace(s, signer, commitHash)
	if err != nil {
		t.Fatalf("SignCommitInPlace: %v", err)
	}

	if newHash == commitHash {
		t.Error("signing should change the commit hash")
	}

	hasSig, err := identity.CommitHasSignature(s, newHash.String())
	if err != nil {
		t.Fatalf("CommitHasSignature: %v", err)
	}
	if !hasSig {
		t.Error("commit should have PGPSignature after signing")
	}
}

func TestSignCommitInPlace_NilSignerIsNoop(t *testing.T) {
	s := newTestStorer(t)
	commitHash := createTestCommit(t, s)

	newHash, err := identity.SignCommitInPlace(s, nil, commitHash)
	if err != nil {
		t.Fatalf("SignCommitInPlace: %v", err)
	}

	if newHash != commitHash {
		t.Error("nil signer should return original hash")
	}

	hasSig, err := identity.CommitHasSignature(s, commitHash.String())
	if err != nil {
		t.Fatalf("CommitHasSignature: %v", err)
	}
	if hasSig {
		t.Error("commit should NOT have PGPSignature without signer")
	}
}

func TestCommitHasSignature_UnsignedCommit(t *testing.T) {
	s := newTestStorer(t)
	commitHash := createTestCommit(t, s)

	hasSig, err := identity.CommitHasSignature(s, commitHash.String())
	if err != nil {
		t.Fatalf("CommitHasSignature: %v", err)
	}
	if hasSig {
		t.Error("unsigned commit should not have signature")
	}
}

func TestCommitHasSignature_BadHash(t *testing.T) {
	s := newTestStorer(t)

	_, err := identity.CommitHasSignature(s, "0000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("expected error for nonexistent commit hash")
	}
}
