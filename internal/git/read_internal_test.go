// Internal tests for ReadFileAtCommit — requires access to unexported helpers.
package git

import (
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestReadFileAtCommit_CaseInsensitiveFallback verifies that ReadFileAtCommit
// resolves a normalised (lowercase) path against a commit that stored the file
// with mixed-case path components (pre-normalisation history).
func TestReadFileAtCommit_CaseInsensitiveFallback(t *testing.T) {
	store := newInternalTestStore(t)

	// Write a file using the internal helper so we can bypass the ToLower
	// enforcement in WriteFile and store an uppercase path — simulating a
	// pre-normalisation commit.
	headRef, err := store.repo.Head()
	if err != nil {
		t.Fatal(err)
	}
	sig := object.Signature{Name: "test", Email: "test@test", When: time.Now()}
	uppercasePath := "kb/Technology/AI/fact.md"
	content := "# Test\nBody.\n"
	commitHash, _, err := writeFileToStore(store.storer, headRef.Hash(), uppercasePath, content, "add uppercase fact", sig, sig)
	if err != nil {
		t.Fatalf("writeFileToStore: %v", err)
	}
	// Update the branch ref so ReadFileAtCommit can find the commit.
	branchRef := plumbing.NewBranchReferenceName(testBranch)
	if err := store.storer.SetReference(plumbing.NewHashReference(branchRef, commitHash)); err != nil {
		t.Fatalf("SetReference: %v", err)
	}

	// ReadFileAtCommit with the normalised lowercase path must succeed.
	lowercasePath := "kb/technology/ai/fact.md"
	got, err := store.ReadFileAtCommit(testBranch, lowercasePath, commitHash.String())
	if err != nil {
		t.Fatalf("ReadFileAtCommit with lowercase path: %v", err)
	}
	if got != content {
		t.Fatalf("content mismatch: got %q, want %q", got, content)
	}

	// Exact uppercase path must still work too.
	got2, err := store.ReadFileAtCommit(testBranch, uppercasePath, commitHash.String())
	if err != nil {
		t.Fatalf("ReadFileAtCommit with exact path: %v", err)
	}
	if got2 != content {
		t.Fatalf("content mismatch (exact): got %q, want %q", got2, content)
	}
}
