package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/stretchr/testify/require"
)

// commitInOutput commits a publisher's own file into the output repo, the way a
// publisher adding a README to their published bundle would.
func commitInOutput(t *testing.T, dir, name, body string) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	wt, err := repo.Worktree()
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644))
	_, err = wt.Add(name)
	require.NoError(t, err)
	sig := &object.Signature{Name: "pub", Email: "pub@example.com", When: kbTime}
	_, err = wt.Commit("docs: "+name, &git.CommitOptions{Author: sig, Committer: sig})
	require.NoError(t, err)
}

// makeBranchAt creates a branch in the output repo pointing at current HEAD, so
// checkoutOutputBranch takes its EXISTING-branch path rather than minting an
// orphan. Only that path performs a real checkout.
func makeBranchAt(t *testing.T, dir, name string) {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	require.NoError(t, err)
	head, err := repo.Head()
	require.NoError(t, err)
	require.NoError(t, repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), head.Hash())))
}

// A publisher's uncommitted work must never be discarded to make a sync
// possible, and go-git's own refusal ("worktree contains unstaged changes")
// names nothing at all — leaving the user to guess which file, in a directory
// that is mostly generated.
func TestCheckoutOutputBranch_PublisherEditBlocksAndIsNamed(t *testing.T) {
	kbDir, _ := newKB(t)
	outDir, _ := cloneKB(t, kbDir)
	commitInOutput(t, outDir, "README.md", "# My knowledge base\n")
	makeBranchAt(t, outDir, "other")

	// The publisher edits their own tracked file and has not committed it.
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "README.md"),
		[]byte("# My knowledge base\n\nWork in progress.\n"), 0o644))

	repo, err := git.PlainOpen(outDir)
	require.NoError(t, err)
	_, err = checkoutOutputBranch(repo, outDir, "other")
	require.Error(t, err)
	require.Contains(t, err.Error(), "README.md", "the blocking file must be named")
	require.Contains(t, err.Error(), "stash", "and the way out must be stated")

	// And the edit is still there: refusing is the point, not a side effect.
	body, rerr := os.ReadFile(filepath.Join(outDir, "README.md"))
	require.NoError(t, rerr)
	require.Contains(t, string(body), "Work in progress.")
}

// An untracked file is not an unstaged change — go-git carries it across a
// branch switch — so it must not be mistaken for one and block the sync.
func TestCheckoutOutputBranch_UntrackedPublisherFileDoesNotBlock(t *testing.T) {
	kbDir, _ := newKB(t)
	outDir, _ := cloneKB(t, kbDir)
	makeBranchAt(t, outDir, "other")
	require.NoError(t, os.WriteFile(filepath.Join(outDir, "NOTES.md"), []byte("draft\n"), 0o644))

	repo, err := git.PlainOpen(outDir)
	require.NoError(t, err)
	created, err := checkoutOutputBranch(repo, outDir, "other")
	require.NoError(t, err)
	require.False(t, created)
	require.FileExists(t, filepath.Join(outDir, "NOTES.md"), "an untracked draft survives the switch")
}

// A dirty file this tool OWNS is a different case: export is about to rewrite
// it from the source commit regardless, so refusing the sync over it would
// block the user on a file that is not theirs and whose content does not
// matter. It is restored, and the switch proceeds.
func TestCheckoutOutputBranch_DirtyBundleFileIsRestoredNotRefused(t *testing.T) {
	kbDir, _ := newKB(t)
	outDir, _ := cloneKB(t, kbDir)
	makeBranchAt(t, outDir, "other")

	owned := filepath.Join(outDir, "index.md")
	require.NoError(t, os.WriteFile(owned, []byte("corrupted by hand\n"), 0o644))

	repo, err := git.PlainOpen(outDir)
	require.NoError(t, err)
	created, err := checkoutOutputBranch(repo, outDir, "other")
	require.NoError(t, err, "a dirty BUNDLE file must not block the switch")
	require.False(t, created)

	body, rerr := os.ReadFile(owned)
	require.NoError(t, rerr)
	require.NotContains(t, string(body), "corrupted by hand")

	head, err := repo.Head()
	require.NoError(t, err)
	require.Equal(t, "other", head.Name().Short(), "the switch actually happened")
}

// The owned-paths boundary applies to the repair too: restoring must refuse a
// path outside it rather than write over a publisher's file from the index.
func TestRestoreOwnedFromIndex_RefusesUnownedPaths(t *testing.T) {
	kbDir, _ := newKB(t)
	outDir, _ := cloneKB(t, kbDir)
	repo, err := git.PlainOpen(outDir)
	require.NoError(t, err)

	err = restoreOwnedFromIndex(repo, outDir, []string{"README.md"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "outside the owned paths")
}
