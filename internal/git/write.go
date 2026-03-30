// Write operations on the git store: single/batch file writes, deletes, and tagging.
package git

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// WriteFile writes content to path in a new commit with message on branch.
// Returns the commit hash and the blob hash of the written file.
func (s *Store) WriteFile(ctx context.Context, branch, path, content, message, operation string) (commitHash string, blobHash string, err error) {
	path = strings.ToLower(path)
	if path == "" {
		return "", "", fmt.Errorf("git: WriteFile: path must not be empty")
	}
	if strings.Contains(path, "..") {
		return "", "", fmt.Errorf("git: WriteFile: path must not contain '..'")
	}

	unlock := s.lockBranch(branch)

	headHash, err := s.resolveRef(ctx, branch)
	if err != nil {
		unlock()
		return "", "", fmt.Errorf("WriteFile: ref: %w", err)
	}

	author := s.authorSig(branch, operation)
	committer := s.committerSig(branch)
	newCommitHash, newBlobHash, err := writeFileToStore(s.storer, headHash, path, content, message, author, committer)
	if err != nil {
		unlock()
		return "", "", err
	}

	if s.signer != nil {
		newCommitHash, err = signCommitInPlace(s.storer, s.signer, newCommitHash)
		if err != nil {
			unlock()
			return "", "", err
		}
	}

	// Update the branch ref to point to the new commit.
	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := s.storer.SetReference(plumbing.NewHashReference(branchRefName, newCommitHash)); err != nil {
		unlock()
		return "", "", err
	}
	unlock()

	// Notify and append commit log outside the lock — notifyCommit triggers
	// index sync which may call back into Store for reads.
	s.notifyCommit(branch, newCommitHash.String())
	s.appendCommitLog(ctx, branch, newCommitHash)
	return newCommitHash.String(), newBlobHash.String(), nil
}

// DeleteFile removes path from branch and creates a commit.
// Returns the commit hash of the new commit.
func (s *Store) DeleteFile(ctx context.Context, branch, path, message, operation string) (commitHash string, err error) {
	path = strings.ToLower(path)
	if path == "" {
		return "", fmt.Errorf("git: DeleteFile: path must not be empty")
	}
	if strings.Contains(path, "..") {
		return "", fmt.Errorf("git: DeleteFile: path must not contain '..'")
	}

	unlock := s.lockBranch(branch)

	headHash, err := s.resolveRef(ctx, branch)
	if err != nil {
		unlock()
		return "", fmt.Errorf("DeleteFile: ref: %w", err)
	}

	// Check existence inside the lock to avoid a TOCTOU race.
	exists, err := s.FileExists(ctx, branch, path)
	if err != nil {
		unlock()
		return "", fmt.Errorf("DeleteFile: check exists: %w", err)
	}
	if !exists {
		unlock()
		return "", fmt.Errorf("DeleteFile: file %q does not exist", path)
	}

	author := s.authorSig(branch, operation)
	committer := s.committerSig(branch)
	newCommitHash, err := deleteFileFromStore(s.storer, headHash, path, message, author, committer)
	if err != nil {
		unlock()
		return "", err
	}

	if s.signer != nil {
		newCommitHash, err = signCommitInPlace(s.storer, s.signer, newCommitHash)
		if err != nil {
			unlock()
			return "", err
		}
	}

	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := s.storer.SetReference(plumbing.NewHashReference(branchRefName, newCommitHash)); err != nil {
		unlock()
		return "", err
	}
	unlock()

	s.notifyCommit(branch, newCommitHash.String())
	s.appendCommitLog(ctx, branch, newCommitHash)
	return newCommitHash.String(), nil
}

// BatchWrite writes multiple files in one commit on branch.
// Returns the commit hash and a map of path → blob hash for each written file.
func (s *Store) BatchWrite(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error) {
	if len(files) == 0 {
		return "", nil, nil
	}

	// Lowercase all paths.
	lowered := make(map[string]string, len(files))
	for path, content := range files {
		lowered[strings.ToLower(path)] = content
	}
	files = lowered

	// Pre-flight validation: reject empty paths and paths containing "..".
	for path := range files {
		if path == "" {
			return "", nil, fmt.Errorf("git: BatchWrite: path must not be empty")
		}
		if strings.Contains(path, "..") {
			return "", nil, fmt.Errorf("git: BatchWrite: path must not contain '..'")
		}
	}

	unlock := s.lockBranch(branch)
	cHash, blobHashes, err := s.batchWriteLocked(ctx, branch, files, message, operation)
	unlock()
	if err != nil {
		return "", nil, err
	}

	// Notify and append commit log outside the lock — notifyCommit triggers
	// index sync which may call back into Store for reads.
	s.notifyCommit(branch, cHash.String())
	s.appendCommitLog(ctx, branch, cHash)
	return cHash.String(), blobHashes, nil
}

// batchWriteLocked performs the actual BatchWrite work. Caller must hold the branch lock.
func (s *Store) batchWriteLocked(ctx context.Context, branch string, files map[string]string, message, operation string) (plumbing.Hash, map[string]string, error) {
	headHash, err := s.resolveRef(ctx, branch)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("BatchWrite: ref: %w", err)
	}

	parentHash := headHash

	// Read existing root tree.
	var rootTree *object.Tree
	if parentHash != plumbing.ZeroHash {
		parentCommit, err := object.GetCommit(s.storer, parentHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("BatchWrite: get parent commit: %w", err)
		}
		rootTree, err = parentCommit.Tree()
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("BatchWrite: get parent tree: %w", err)
		}
	}

	blobHashes := make(map[string]string, len(files))

	// Apply each file to the tree sequentially.
	var currentRootHash plumbing.Hash
	for path, content := range files {
		// Create blob.
		blobObj := s.storer.NewEncodedObject()
		blobObj.SetType(plumbing.BlobObject)
		bw, err := blobObj.Writer()
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("BatchWrite: blob writer for %q: %w", path, err)
		}
		if _, err := io.WriteString(bw, content); err != nil {
			bw.Close()
			return plumbing.ZeroHash, nil, fmt.Errorf("BatchWrite: blob write for %q: %w", path, err)
		}
		bw.Close()
		blobHash, err := s.storer.SetEncodedObject(blobObj)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("BatchWrite: store blob for %q: %w", path, err)
		}
		blobHashes[path] = blobHash.String()

		// Update tree.
		currentRootHash, err = buildTree(s.storer, rootTree, path, blobHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("BatchWrite: build tree for %q: %w", path, err)
		}

		// Load updated root tree for next iteration.
		rootTree, err = object.GetTree(s.storer, currentRootHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("BatchWrite: get updated tree: %w", err)
		}
	}

	// Create single commit.
	author := s.authorSig(branch, operation)
	committer := s.committerSig(branch)
	commit := &object.Commit{
		Author:    author,
		Committer: committer,
		Message:   message,
		TreeHash:  currentRootHash,
	}
	if parentHash != plumbing.ZeroHash {
		commit.ParentHashes = []plumbing.Hash{parentHash}
	}

	commitObj := s.storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("BatchWrite: encode commit: %w", err)
	}
	cHash, err := s.storer.SetEncodedObject(commitObj)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("BatchWrite: store commit: %w", err)
	}

	if s.signer != nil {
		cHash, err = signCommitInPlace(s.storer, s.signer, cHash)
		if err != nil {
			return plumbing.ZeroHash, nil, err
		}
	}

	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := s.storer.SetReference(plumbing.NewHashReference(branchRefName, cHash)); err != nil {
		return plumbing.ZeroHash, nil, err
	}
	return cHash, blobHashes, nil
}

// Tag creates a lightweight tag ref at the tip of branch.
func (s *Store) Tag(ctx context.Context, branch, name string) error {
	headHash, err := s.resolveRef(ctx, branch)
	if err != nil {
		return fmt.Errorf("Tag: ref: %w", err)
	}

	tagRefName := plumbing.NewTagReferenceName(name)
	return s.storer.SetReference(plumbing.NewHashReference(tagRefName, headHash))
}

// TagsContaining returns tag names whose target is reachable from hash.
// Optimization: collect all commits reachable from hash in one walk, then
// check each tag's target against that set — O(depth + tags) instead of
// O(tags * depth).
func (s *Store) TagsContaining(ctx context.Context, hash string) ([]string, error) {
	targetHash := plumbing.NewHash(hash)

	// Build set of all commits reachable from targetHash (one walk).
	reachable := make(map[plumbing.Hash]bool)
	logIter, err := s.repo.Log(&gogit.LogOptions{From: targetHash})
	if err != nil {
		return nil, fmt.Errorf("TagsContaining: log from target: %w", err)
	}
	_ = logIter.ForEach(func(c *object.Commit) error {
		reachable[c.Hash] = true
		return nil
	})
	logIter.Close()

	refIter, err := s.storer.IterReferences()
	if err != nil {
		return nil, fmt.Errorf("TagsContaining: iter refs: %w", err)
	}
	defer refIter.Close()

	var tags []string
	err = refIter.ForEach(func(ref *plumbing.Reference) error {
		if !strings.HasPrefix(ref.Name().String(), "refs/tags/") {
			return nil
		}
		if reachable[ref.Hash()] {
			tagName := strings.TrimPrefix(ref.Name().String(), "refs/tags/")
			tags = append(tags, tagName)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("TagsContaining: %w", err)
	}

	sort.Strings(tags)
	return tags, nil
}
