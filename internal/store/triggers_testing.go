package store

import (
	"context"
	"fmt"
	"sort"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestingSetRef moves (or creates) a git reference WITHOUT a commit and without
// notifyCommit — for tests in sibling packages that need a ref shape the write
// path never produces on demand: an agent branch rewound so its old head is no
// longer an ancestor (the replay case), or main moved to a commit as if the
// acceptance gate had advanced it.
//
// Test-only by contract, like the store's fallback-signer hook: it panics outside a test
// binary, so no production path can move a ref behind the store's back.
func (s *Service) TestingSetRef(name, hash string) error {
	if !testing.Testing() {
		panic("store.TestingSetRef called outside a test binary")
	}
	if !plumbing.IsHash(hash) {
		return fmt.Errorf("TestingSetRef: %q is not a commit hash", hash)
	}
	return s.rh.gits.SetReference(plumbing.NewHashReference(plumbing.ReferenceName(name), plumbing.NewHash(hash)))
}

// TestingDeleteRef removes a git reference WITHOUT notifyCommit — for a test
// that needs a repo with no upstream branch (F07 PR 5: no main means no repo
// recipe tier). Test-only by contract (see TestingSetRef).
func (s *Service) TestingDeleteRef(name string) error {
	if !testing.Testing() {
		panic("store.TestingDeleteRef called outside a test binary")
	}
	return s.rh.gits.RemoveReference(plumbing.ReferenceName(name))
}

// TestingCommitFiles commits files onto branch as ONE signed commit, moving
// the ref but skipping notifyCommit — no commit_log row, no index sync, no
// observer. It exists for a test that needs a very large advance (thousands of
// fact paths) without paying the index's cost for it: the dispatcher's own
// work is what such a test measures. Test-only by contract (see
// TestingSetRef). Returns the commit hash.
func (s *Service) TestingCommitFiles(branch string, files map[string]string, message string) (string, error) {
	if !testing.Testing() {
		panic("store.TestingCommitFiles called outside a test binary")
	}
	rh := s.rh
	ctx := context.Background()
	parent, err := rh.resolveRef(ctx, branch)
	if err != nil {
		return "", err
	}
	parentCommit, err := rh.repo.CommitObject(parent)
	if err != nil {
		return "", fmt.Errorf("TestingCommitFiles: parent: %w", err)
	}
	tree, err := parentCommit.Tree()
	if err != nil {
		return "", fmt.Errorf("TestingCommitFiles: parent tree: %w", err)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	root := parentCommit.TreeHash
	for _, p := range paths {
		blob, err := writeBlobToStore(rh.gits, []byte(files[p]))
		if err != nil {
			return "", err
		}
		if root, err = buildTree(rh.gits, tree, p, blob); err != nil {
			return "", fmt.Errorf("TestingCommitFiles: tree %q: %w", p, err)
		}
		if tree, err = object.GetTree(rh.gits, root); err != nil {
			return "", fmt.Errorf("TestingCommitFiles: reread tree: %w", err)
		}
	}
	signer, err := rh.commitSigner()
	if err != nil {
		return "", err
	}
	author, committer, err := rh.commitSigs(ctx, branch, "learn")
	if err != nil {
		return "", err
	}
	c := &object.Commit{
		Author:       author,
		Committer:    committer,
		Message:      message,
		TreeHash:     root,
		ParentHashes: []plumbing.Hash{parent},
	}
	h, err := storeCommit(rh.gits, signer, c)
	if err != nil {
		return "", err
	}
	if err := rh.gits.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)); err != nil {
		return "", err
	}
	return h.String(), nil
}
