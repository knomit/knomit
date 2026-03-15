package git_test

import (
	"path/filepath"
	"testing"

	"knomit/internal/git"
)

func TestSignCommitInPlace_SignsAndChangesHash(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	signer := generateTestSigner(t)
	store.SetSigner(signer)

	commitHash, _, err := store.WriteFile("kb/test.md", "# Test\n", "add test")
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	hasSig, err := git.CommitHasSignature(store, commitHash)
	if err != nil {
		t.Fatalf("CommitHasSignature: %v", err)
	}
	if !hasSig {
		t.Error("commit should have PGPSignature after signing")
	}
}

func TestSignCommitInPlace_NoSignerNoSignature(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// No signer — commit should be unsigned.
	commitHash, _, err := store.WriteFile("kb/test.md", "# Test\n", "add test")
	if err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	hasSig, err := git.CommitHasSignature(store, commitHash)
	if err != nil {
		t.Fatalf("CommitHasSignature: %v", err)
	}
	if hasSig {
		t.Error("commit should NOT have PGPSignature without signer")
	}
}
