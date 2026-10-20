package store

import (
	"context"
	"fmt"
	"knomit/internal/fact"
	"knomit/internal/store/facts"
	"knomit/internal/store/internal/vcs"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// TestingSetRef moves (or creates) a git reference WITHOUT a commit and without
// NotifyCommit — for tests in sibling packages that need a ref shape the write
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
	return s.rh.Storer().SetReference(plumbing.NewHashReference(plumbing.ReferenceName(name), plumbing.NewHash(hash)))
}

// TestingDeleteRef removes a git reference WITHOUT NotifyCommit — for a test
// that needs a repo with no upstream branch (F07 PR 5: no main means no repo
// recipe tier). Test-only by contract (see TestingSetRef).
func (s *Service) TestingDeleteRef(name string) error {
	if !testing.Testing() {
		panic("store.TestingDeleteRef called outside a test binary")
	}
	return s.rh.Storer().RemoveReference(plumbing.ReferenceName(name))
}

// TestingCommitFiles commits files onto branch as ONE signed commit, moving
// the ref but skipping NotifyCommit — no commit_log row, no index sync, no
// observer. It exists for a test that needs a very large advance (thousands of
// fact paths) without paying the index's cost for it: the dispatcher's own
// work is what such a test measures. Test-only by contract (see
// TestingSetRef). Returns the commit hash.
//
// The trees are built in ONE pass (testingTree): each tree object the commit
// needs is written once. Applying the paths one at a time with BuildTree, as
// the write path does for its handful of files, rewrites every tree on the
// path for every file: about five tree objects per file, 60,000 loose objects
// for the 12,000-file advance of TestDispatch_Tx1Bounded, which cost that test
// ~15 s on the Windows runner. The resulting tree is the same object either
// way (TestTestingCommitFiles_TreeMatchesPerPathBuild).
func (s *Service) TestingCommitFiles(branch string, files map[string]string, message string) (string, error) {
	if !testing.Testing() {
		panic("store.TestingCommitFiles called outside a test binary")
	}
	rh := s.rh
	ctx := context.Background()
	parent, err := rh.ResolveRef(ctx, branch)
	if err != nil {
		return "", err
	}
	parentCommit, err := rh.Repository().CommitObject(parent)
	if err != nil {
		return "", fmt.Errorf("TestingCommitFiles: parent: %w", err)
	}
	tree, err := parentCommit.Tree()
	if err != nil {
		return "", fmt.Errorf("TestingCommitFiles: parent tree: %w", err)
	}
	root, err := testingTree(rh.Storer(), tree, files)
	if err != nil {
		return "", fmt.Errorf("TestingCommitFiles: %w", err)
	}
	signer, err := rh.CommitSigner()
	if err != nil {
		return "", err
	}
	author, committer, err := rh.CommitSigs(ctx, branch, "learn")
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
	h, err := vcs.StoreCommit(rh.Storer(), signer, c)
	if err != nil {
		return "", err
	}
	if err := rh.Storer().SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName(branch), h)); err != nil {
		return "", err
	}
	return h.String(), nil
}

// Triggers returns the trigger dispatcher's store surface.
func (s *Service) Triggers() TriggerIndex { return s.rh }

// Templates returns the template reader of this store.
func (s *Service) Templates() TemplateIndex { return s.rh }

// WriteSystemTree writes files as ONE authored commit on branch, KEEPING THE
// CASE of every path. It is the door F24's create-from-template writes a
// template through: every fact door lowercases a nested path, which would
// turn .knomit/skills/x/SKILL.md into skill.md and README.md into readme.md.
//
// Because it goes around that normalization, it accepts the narrowest set
// that works and nothing else: README.md and paths under .knomit/
// (fact.TemplatePathAllowed), no two paths that differ only by case. It is
// never on the MCP surface. Otherwise it is batchWrite: read-only refused,
// validatePath, the signer taken before any object is written, the ctx
// trailers stamped, NotifyCommit under the branch lock.
func (s *Service) WriteSystemTree(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error) {
	fi := s.fi
	if s.rh.ReadOnly() {
		return "", nil, ErrRepoReadOnly
	}
	if len(files) == 0 {
		return "", nil, fmt.Errorf("%w: nothing to write", ErrSystemTreePath)
	}
	seen := make(map[string]string, len(files))
	for p := range files {
		if !fact.TemplatePathAllowed(p) {
			return "", nil, fmt.Errorf("%w: %q", ErrSystemTreePath, p)
		}
		if other, dup := seen[strings.ToLower(p)]; dup {
			return "", nil, fmt.Errorf("%w: %q and %q differ only by case", ErrSystemTreePath, other, p)
		}
		seen[strings.ToLower(p)] = p
		if err := facts.ValidatePath(p); err != nil {
			return "", nil, fmt.Errorf("store: WriteSystemTree: %w", err)
		}
	}
	unlock := s.rh.LockBranch(branch)
	defer unlock()
	cHash, blobHashes, err := fi.BatchWriteLocked(ctx, branch, files, nil, nil, message, operation)
	if err != nil {
		return "", nil, err
	}
	if err := s.rh.NotifyCommit(ctx, branch, cHash); err != nil {
		return "", nil, err
	}
	return cHash.String(), blobHashes, nil
}

// SystemFiles returns the system-file reader of this store.
func (s *Service) SystemFiles() SystemFileIndex { return s.rh }

// Skills returns the skill reader of this store.
func (s *Service) Skills() SkillIndex { return s.rh }

// SignerFingerprint is the full pki.Fingerprint of the key this store signs
// authored commits with, or "" when it has none. See repoHandler.signerFingerprint.
func (s *Service) SignerFingerprint() string { return s.rh.SignerFingerprint() }
