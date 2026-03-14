// Write operations on the git store: single/batch file writes, deletes, and tagging.
package git

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// WriteFile writes content to path in a new commit with message.
// Returns the commit hash and the blob hash of the written file.
func (s *Store) WriteFile(path, content, message string) (commitHash string, blobHash string, err error) {
	if path == "" {
		return "", "", fmt.Errorf("git: WriteFile: path must not be empty")
	}
	if strings.Contains(path, "..") {
		return "", "", fmt.Errorf("git: WriteFile: path must not contain '..'")
	}

	headRef, err := s.repo.Head()
	if err != nil {
		return "", "", fmt.Errorf("WriteFile: head: %w", err)
	}

	newCommitHash, newBlobHash, err := writeFileToStore(s.storer, headRef.Hash(), path, content, message)
	if err != nil {
		return "", "", err
	}

	// Update the branch ref to point to the new commit.
	branchRefName := plumbing.NewBranchReferenceName(s.branch)
	newRef := plumbing.NewHashReference(branchRefName, newCommitHash)
	if err := s.storer.SetReference(newRef); err != nil {
		return "", "", err
	}
	s.notifyCommit(newCommitHash.String())
	return newCommitHash.String(), newBlobHash.String(), nil
}

// DeleteFile removes path from HEAD and creates a commit.
// Returns the commit hash of the new commit.
func (s *Store) DeleteFile(path, message string) (commitHash string, err error) {
	if path == "" {
		return "", fmt.Errorf("git: DeleteFile: path must not be empty")
	}
	if strings.Contains(path, "..") {
		return "", fmt.Errorf("git: DeleteFile: path must not contain '..'")
	}

	headRef, err := s.repo.Head()
	if err != nil {
		return "", fmt.Errorf("DeleteFile: head: %w", err)
	}

	newCommitHash, err := deleteFileFromStore(s.storer, headRef.Hash(), path, message)
	if err != nil {
		return "", err
	}

	branchRefName := plumbing.NewBranchReferenceName(s.branch)
	newRef := plumbing.NewHashReference(branchRefName, newCommitHash)
	if err := s.storer.SetReference(newRef); err != nil {
		return "", err
	}
	s.notifyCommit(newCommitHash.String())
	return newCommitHash.String(), nil
}

// BatchWrite writes multiple files in one commit.
// Returns the commit hash and a map of path → blob hash for each written file.
func (s *Store) BatchWrite(files map[string]string, message string) (commitHash string, blobHashes map[string]string, err error) {
	if len(files) == 0 {
		return "", nil, nil
	}

	// Pre-flight validation: reject empty paths and paths containing "..".
	for path := range files {
		if path == "" {
			return "", nil, fmt.Errorf("git: BatchWrite: path must not be empty")
		}
		if strings.Contains(path, "..") {
			return "", nil, fmt.Errorf("git: BatchWrite: path must not contain '..'")
		}
	}

	headRef, err := s.repo.Head()
	if err != nil {
		return "", nil, fmt.Errorf("BatchWrite: head: %w", err)
	}

	parentHash := headRef.Hash()

	// Read existing root tree.
	var rootTree *object.Tree
	if parentHash != plumbing.ZeroHash {
		parentCommit, err := object.GetCommit(s.storer, parentHash)
		if err != nil {
			return "", nil, fmt.Errorf("BatchWrite: get parent commit: %w", err)
		}
		rootTree, err = parentCommit.Tree()
		if err != nil {
			return "", nil, fmt.Errorf("BatchWrite: get parent tree: %w", err)
		}
	}

	blobHashes = make(map[string]string, len(files))

	// Apply each file to the tree sequentially.
	var currentRootHash plumbing.Hash
	for path, content := range files {
		// Create blob.
		blobObj := s.storer.NewEncodedObject()
		blobObj.SetType(plumbing.BlobObject)
		bw, err := blobObj.Writer()
		if err != nil {
			return "", nil, fmt.Errorf("BatchWrite: blob writer for %q: %w", path, err)
		}
		if _, err := io.WriteString(bw, content); err != nil {
			bw.Close()
			return "", nil, fmt.Errorf("BatchWrite: blob write for %q: %w", path, err)
		}
		bw.Close()
		blobHash, err := s.storer.SetEncodedObject(blobObj)
		if err != nil {
			return "", nil, fmt.Errorf("BatchWrite: store blob for %q: %w", path, err)
		}
		blobHashes[path] = blobHash.String()

		// Update tree.
		currentRootHash, err = buildTree(s.storer, rootTree, path, blobHash)
		if err != nil {
			return "", nil, fmt.Errorf("BatchWrite: build tree for %q: %w", path, err)
		}

		// Load updated root tree for next iteration.
		rootTree, err = object.GetTree(s.storer, currentRootHash)
		if err != nil {
			return "", nil, fmt.Errorf("BatchWrite: get updated tree: %w", err)
		}
	}

	// Create single commit.
	now := time.Now()
	sig := object.Signature{
		Name:  "knomit",
		Email: "knomit@local",
		When:  now,
	}
	commit := &object.Commit{
		Author:    sig,
		Committer: sig,
		Message:   message,
		TreeHash:  currentRootHash,
	}
	if parentHash != plumbing.ZeroHash {
		commit.ParentHashes = []plumbing.Hash{parentHash}
	}

	commitObj := s.storer.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return "", nil, fmt.Errorf("BatchWrite: encode commit: %w", err)
	}
	cHash, err := s.storer.SetEncodedObject(commitObj)
	if err != nil {
		return "", nil, fmt.Errorf("BatchWrite: store commit: %w", err)
	}

	branchRefName := plumbing.NewBranchReferenceName(s.branch)
	newRef := plumbing.NewHashReference(branchRefName, cHash)
	if err := s.storer.SetReference(newRef); err != nil {
		return "", nil, err
	}
	s.notifyCommit(cHash.String())
	return cHash.String(), blobHashes, nil
}

// Tag creates a lightweight tag ref at HEAD.
func (s *Store) Tag(name string) error {
	headRef, err := s.repo.Head()
	if err != nil {
		return fmt.Errorf("Tag: head: %w", err)
	}

	tagRefName := plumbing.NewTagReferenceName(name)
	tagRef := plumbing.NewHashReference(tagRefName, headRef.Hash())
	return s.storer.SetReference(tagRef)
}

// TagsContaining returns tag names whose target is reachable from hash.
// Optimization: collect all commits reachable from hash in one walk, then
// check each tag's target against that set — O(depth + tags) instead of
// O(tags * depth).
func (s *Store) TagsContaining(hash string) ([]string, error) {
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
