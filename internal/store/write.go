// Write operations on the git store: single/batch file writes, deletes, and tagging.
package store

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
func (s *Service) writeFile(ctx context.Context, branch, path, content, message, operation string) (commitHash string, blobHash string, err error) {
	path = strings.ToLower(path)
	if path == "" {
		return "", "", fmt.Errorf("store: WriteFile: path must not be empty")
	}
	if strings.Contains(path, "..") {
		return "", "", fmt.Errorf("store: WriteFile: path must not contain '..'")
	}

	unlock := s.lockBranch(branch)

	headHash, err := s.resolveRef(ctx, branch)
	if err != nil {
		unlock()
		return "", "", fmt.Errorf("WriteFile: ref: %w", err)
	}

	author := s.authorSig(branch, operation)
	committer := s.committerSig(branch)
	newCommitHash, newBlobHash, err := writeFileToStore(s.gits, headHash, path, content, message, author, committer)
	if err != nil {
		unlock()
		return "", "", err
	}

	newCommitHash, err = signCommitInPlace(s.gits, s.signer, newCommitHash)
	if err != nil {
		unlock()
		return "", "", err
	}

	// Update the branch ref to point to the new commit.
	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := s.gits.SetReference(plumbing.NewHashReference(branchRefName, newCommitHash)); err != nil {
		unlock()
		return "", "", err
	}
	unlock()

	// Notify outside the lock — appendCommitLog triggers index sync which
	// may call back into Service for reads.
	s.notifyCommit(ctx, branch, newCommitHash)
	return newCommitHash.String(), newBlobHash.String(), nil
}

// DeleteFile removes path from branch and creates a commit.
// Returns the commit hash of the new commit.
func (s *Service) deleteFile(ctx context.Context, branch, path, message, operation string) (commitHash string, err error) {
	path = strings.ToLower(path)
	if path == "" {
		return "", fmt.Errorf("store: DeleteFile: path must not be empty")
	}
	if strings.Contains(path, "..") {
		return "", fmt.Errorf("store: DeleteFile: path must not contain '..'")
	}

	unlock := s.lockBranch(branch)

	headHash, err := s.resolveRef(ctx, branch)
	if err != nil {
		unlock()
		return "", fmt.Errorf("DeleteFile: ref: %w", err)
	}

	// Check existence inside the lock to avoid a TOCTOU race.
	exists, err := s.fileExists(ctx, branch, path)
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
	newCommitHash, err := deleteFileFromStore(s.gits, headHash, path, message, author, committer)
	if err != nil {
		unlock()
		return "", err
	}

	newCommitHash, err = signCommitInPlace(s.gits, s.signer, newCommitHash)
	if err != nil {
		unlock()
		return "", err
	}

	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := s.gits.SetReference(plumbing.NewHashReference(branchRefName, newCommitHash)); err != nil {
		unlock()
		return "", err
	}
	unlock()

	s.notifyCommit(ctx, branch, newCommitHash)
	return newCommitHash.String(), nil
}

// BatchWrite writes multiple files in one commit on branch.
// Returns the commit hash and a map of path → blob hash for each written file.
func (s *Service) batchWrite(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error) {
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
			return "", nil, fmt.Errorf("store: batchWrite: path must not be empty")
		}
		if strings.Contains(path, "..") {
			return "", nil, fmt.Errorf("store: batchWrite: path must not contain '..'")
		}
	}

	unlock := s.lockBranch(branch)
	cHash, blobHashes, err := s.batchWriteLocked(ctx, branch, files, message, operation)
	unlock()
	if err != nil {
		return "", nil, err
	}

	s.notifyCommit(ctx, branch, cHash)
	return cHash.String(), blobHashes, nil
}

// batchWriteLocked performs the actual batchWrite work. Caller must hold the branch lock.
func (s *Service) batchWriteLocked(ctx context.Context, branch string, files map[string]string, message, operation string) (plumbing.Hash, map[string]string, error) {
	headHash, err := s.resolveRef(ctx, branch)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: ref: %w", err)
	}

	parentHash := headHash

	// Read existing root tree.
	var rootTree *object.Tree
	if parentHash != plumbing.ZeroHash {
		parentCommit, err := object.GetCommit(s.gits, parentHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: get parent commit: %w", err)
		}
		rootTree, err = parentCommit.Tree()
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: get parent tree: %w", err)
		}
	}

	blobHashes := make(map[string]string, len(files))

	// Apply each file to the tree sequentially.
	var currentRootHash plumbing.Hash
	for path, content := range files {
		// Create blob.
		blobObj := s.gits.NewEncodedObject()
		blobObj.SetType(plumbing.BlobObject)
		bw, err := blobObj.Writer()
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: blob writer for %q: %w", path, err)
		}
		if _, err := io.WriteString(bw, content); err != nil {
			bw.Close()
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: blob write for %q: %w", path, err)
		}
		bw.Close()
		blobHash, err := s.gits.SetEncodedObject(blobObj)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: store blob for %q: %w", path, err)
		}
		blobHashes[path] = blobHash.String()

		// Update tree.
		currentRootHash, err = buildTree(s.gits, rootTree, path, blobHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: build tree for %q: %w", path, err)
		}

		// Load updated root tree for next iteration.
		rootTree, err = object.GetTree(s.gits, currentRootHash)
		if err != nil {
			return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: get updated tree: %w", err)
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

	commitObj := s.gits.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: encode commit: %w", err)
	}
	cHash, err := s.gits.SetEncodedObject(commitObj)
	if err != nil {
		return plumbing.ZeroHash, nil, fmt.Errorf("batchWrite: store commit: %w", err)
	}

	cHash, err = signCommitInPlace(s.gits, s.signer, cHash)
	if err != nil {
		return plumbing.ZeroHash, nil, err
	}

	branchRefName := plumbing.NewBranchReferenceName(branch)
	if err := s.gits.SetReference(plumbing.NewHashReference(branchRefName, cHash)); err != nil {
		return plumbing.ZeroHash, nil, err
	}
	return cHash, blobHashes, nil
}

// WriteFact writes a fact to the store and returns the commit and blob hashes.
func (s *Service) WriteFact(ctx context.Context, branch, path, content, message, operation string) (WriteFactResult, error) {
	commitHash, blobHash, err := s.writeFile(ctx, branch, path, content, message, operation)
	if err != nil {
		return WriteFactResult{}, err
	}
	return WriteFactResult{CommitHash: commitHash, BlobHash: blobHash}, nil
}

// DeleteFact deletes a fact and syncs the index so the deletion is immediately visible.
func (s *Service) DeleteFact(ctx context.Context, branch, path, message string) (string, error) {
	commitHash, err := s.deleteFile(ctx, branch, path, message, "retract")
	if err != nil {
		return "", fmt.Errorf("DeleteFact git: %w", err)
	}
	if err := s.idx.Sync(ctx, s, branch); err != nil {
		return "", fmt.Errorf("DeleteFact sync: %w", err)
	}
	return commitHash, nil
}

// BatchWriteFacts writes multiple facts in a single commit.
func (s *Service) BatchWriteFacts(ctx context.Context, branch string, files map[string]string, message, operation string) (commitHash string, blobHashes map[string]string, err error) {
	return s.batchWrite(ctx, branch, files, message, operation)
}

// tag creates a lightweight tag ref at the tip of branch.
func (s *Service) tag(ctx context.Context, branch, name string) error {
	headHash, err := s.resolveRef(ctx, branch)
	if err != nil {
		return fmt.Errorf("tag: ref: %w", err)
	}

	tagRefName := plumbing.NewTagReferenceName(name)
	return s.gits.SetReference(plumbing.NewHashReference(tagRefName, headHash))
}

// tagsContaining returns tag names whose target is reachable from hash.
func (s *Service) tagsContaining(ctx context.Context, hash string) ([]string, error) {
	targetHash := plumbing.NewHash(hash)

	// Build set of all commits reachable from targetHash (one walk).
	reachable := make(map[plumbing.Hash]bool)
	logIter, err := s.repo.Log(&gogit.LogOptions{From: targetHash})
	if err != nil {
		return nil, fmt.Errorf("tagsContaining: log from target: %w", err)
	}
	_ = logIter.ForEach(func(c *object.Commit) error {
		reachable[c.Hash] = true
		return nil
	})
	logIter.Close()

	refIter, err := s.gits.IterReferences()
	if err != nil {
		return nil, fmt.Errorf("tagsContaining: iter refs: %w", err)
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
		return nil, fmt.Errorf("tagsContaining: %w", err)
	}

	sort.Strings(tags)
	return tags, nil
}
