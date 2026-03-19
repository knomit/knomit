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

	commitHash, _, err := store.WriteFile("kb/test.md", "# Test\n", "add test", "learn")
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

func TestDeleteFile_SignsCommit(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	signer := generateTestSigner(t)
	store.SetSigner(signer)

	// Write a file first.
	store.WriteFile("kb/temp.md", "# Temp\n", "add temp", "learn")

	// Delete it — should also produce a signed commit.
	commitHash, err := store.DeleteFile("kb/temp.md", "delete temp", "retract")
	if err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	hasSig, err := git.CommitHasSignature(store, commitHash)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSig {
		t.Error("delete commit should be signed")
	}
}

func TestBatchWrite_SignsCommit(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	signer := generateTestSigner(t)
	store.SetSigner(signer)

	files := map[string]string{
		"kb/a.md": "# A\n",
		"kb/b.md": "# B\n",
	}
	commitHash, _, err := store.BatchWrite(files, "batch add", "learn")
	if err != nil {
		t.Fatalf("BatchWrite: %v", err)
	}

	hasSig, err := git.CommitHasSignature(store, commitHash)
	if err != nil {
		t.Fatal(err)
	}
	if !hasSig {
		t.Error("batch commit should be signed")
	}
}

func TestInitCommits_AreUnsigned(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Init commit (before any signer is set) should be unsigned.
	headHash, err := store.HeadCommit()
	if err != nil {
		t.Fatal(err)
	}

	hasSig, err := git.CommitHasSignature(store, headHash)
	if err != nil {
		t.Fatal(err)
	}
	if hasSig {
		t.Error("init commit should NOT be signed (no signer at init time)")
	}
}

func TestCommitHasSignature_BadHash(t *testing.T) {
	dir := t.TempDir()
	store, err := git.Init(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	_, err = git.CommitHasSignature(store, "0000000000000000000000000000000000000000")
	if err == nil {
		t.Fatal("expected error for nonexistent commit hash")
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
	commitHash, _, err := store.WriteFile("kb/test.md", "# Test\n", "add test", "learn")
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
