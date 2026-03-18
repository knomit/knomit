// Low-level git plumbing: blob creation, tree building/modification, and commit
// creation. These helpers are used by the higher-level read/write/sync operations.
package git

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	storegit "knomit/internal/store/git"
)

// writeFileToStore creates a blob+tree+commit for path/content.
// parentCommitHash is ZeroHash for the initial commit (no parent).
// Returns (commitHash, blobHash, error).
func writeFileToStore(s *storegit.Storer, parentCommitHash plumbing.Hash, path, content, message string, author, committer object.Signature) (plumbing.Hash, plumbing.Hash, error) {
	// 1. Create blob.
	blobObj := s.NewEncodedObject()
	blobObj.SetType(plumbing.BlobObject)
	bw, err := blobObj.Writer()
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("writeFileToStore: blob writer: %w", err)
	}
	if _, err := io.WriteString(bw, content); err != nil {
		bw.Close()
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("writeFileToStore: blob write: %w", err)
	}
	bw.Close()
	blobHash, err := s.SetEncodedObject(blobObj)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("writeFileToStore: store blob: %w", err)
	}

	// 2. Read existing root tree (if any).
	var existingTree *object.Tree
	if parentCommitHash != plumbing.ZeroHash {
		parentCommit, err := object.GetCommit(s, parentCommitHash)
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("writeFileToStore: get parent commit: %w", err)
		}
		existingTree, err = parentCommit.Tree()
		if err != nil {
			return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("writeFileToStore: get parent tree: %w", err)
		}
	}

	// 3. Build new root tree with path added/replaced.
	newRootHash, err := buildTree(s, existingTree, path, blobHash)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("writeFileToStore: build tree: %w", err)
	}

	// 4. Create commit object.
	commit := &object.Commit{
		Author:    author,
		Committer: committer,
		Message:   message,
		TreeHash:  newRootHash,
	}
	if parentCommitHash != plumbing.ZeroHash {
		commit.ParentHashes = []plumbing.Hash{parentCommitHash}
	}

	commitObj := s.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("writeFileToStore: encode commit: %w", err)
	}
	commitHash, err := s.SetEncodedObject(commitObj)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("writeFileToStore: store commit: %w", err)
	}

	return commitHash, blobHash, nil
}

// buildTree constructs a new root tree by adding/replacing path (which may be
// nested, e.g. "general/technology/go/abc123.md") with blobHash. existing may be nil for an
// empty tree. The function recurses through path segments, creating or updating
// subtrees as needed.
func buildTree(s *storegit.Storer, existing *object.Tree, path string, blobHash plumbing.Hash) (plumbing.Hash, error) {
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]

	if len(parts) == 1 {
		// Leaf: insert/replace the file entry in this tree.
		return upsertEntry(s, existing, object.TreeEntry{
			Name: name,
			Mode: filemode.Regular,
			Hash: blobHash,
		})
	}

	// Recurse: find existing subtree for parts[0], recurse into it.
	rest := parts[1]
	var subtree *object.Tree
	if existing != nil {
		for _, e := range existing.Entries {
			if e.Name == name && e.Mode == filemode.Dir {
				var err error
				subtree, err = object.GetTree(s, e.Hash)
				if err != nil {
					return plumbing.ZeroHash, fmt.Errorf("buildTree: get subtree %q: %w", name, err)
				}
				break
			}
		}
	}

	subHash, err := buildTree(s, subtree, rest, blobHash)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return upsertEntry(s, existing, object.TreeEntry{
		Name: name,
		Mode: filemode.Dir,
		Hash: subHash,
	})
}

// upsertEntry adds or replaces entry in a copy of existing (nil means empty tree),
// encodes the resulting tree, stores it, and returns its hash.
func upsertEntry(s *storegit.Storer, existing *object.Tree, entry object.TreeEntry) (plumbing.Hash, error) {
	var entries []object.TreeEntry

	if existing != nil {
		for _, e := range existing.Entries {
			if e.Name != entry.Name {
				entries = append(entries, e)
			}
		}
	}
	entries = append(entries, entry)

	// go-git requires entries to be sorted.
	sort.Sort(object.TreeEntrySorter(entries))

	tree := &object.Tree{Entries: entries}
	treeObj := s.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("upsertEntry: encode tree: %w", err)
	}
	hash, err := s.SetEncodedObject(treeObj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("upsertEntry: store tree: %w", err)
	}
	return hash, nil
}

// deleteFileFromStore creates a commit that removes path from the tree rooted
// at parentCommitHash.
func deleteFileFromStore(s *storegit.Storer, parentCommitHash plumbing.Hash, path, message string, author, committer object.Signature) (plumbing.Hash, error) {
	parentCommit, err := object.GetCommit(s, parentCommitHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: get parent commit: %w", err)
	}
	existingTree, err := parentCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: get parent tree: %w", err)
	}

	newRootHash, err := deleteFromTree(s, existingTree, path)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: delete from tree: %w", err)
	}

	commit := &object.Commit{
		Author:       author,
		Committer:    committer,
		Message:      message,
		TreeHash:     newRootHash,
		ParentHashes: []plumbing.Hash{parentCommitHash},
	}

	commitObj := s.NewEncodedObject()
	if err := commit.Encode(commitObj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: encode commit: %w", err)
	}
	commitHash, err := s.SetEncodedObject(commitObj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: store commit: %w", err)
	}
	return commitHash, nil
}

// deleteFromTree removes path from the tree rooted at existing, recursing
// through directory segments as needed.
func deleteFromTree(s *storegit.Storer, existing *object.Tree, path string) (plumbing.Hash, error) {
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]

	if len(parts) == 1 {
		// Leaf: remove the entry from this tree.
		return removeEntry(s, existing, name)
	}

	// Recurse into subtree.
	rest := parts[1]
	var subtree *object.Tree
	for _, e := range existing.Entries {
		if e.Name == name && e.Mode == filemode.Dir {
			var err error
			subtree, err = object.GetTree(s, e.Hash)
			if err != nil {
				return plumbing.ZeroHash, fmt.Errorf("deleteFromTree: get subtree %q: %w", name, err)
			}
			break
		}
	}
	if subtree == nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFromTree: subtree %q not found", name)
	}

	subHash, err := deleteFromTree(s, subtree, rest)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return upsertEntry(s, existing, object.TreeEntry{
		Name: name,
		Mode: filemode.Dir,
		Hash: subHash,
	})
}

// removeEntry removes the entry with name from a copy of existing tree,
// encodes, stores and returns the new tree hash.
func removeEntry(s *storegit.Storer, existing *object.Tree, name string) (plumbing.Hash, error) {
	var entries []object.TreeEntry
	if existing != nil {
		for _, e := range existing.Entries {
			if e.Name != name {
				entries = append(entries, e)
			}
		}
	}

	sort.Sort(object.TreeEntrySorter(entries))
	tree := &object.Tree{Entries: entries}
	treeObj := s.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("removeEntry: encode tree: %w", err)
	}
	hash, err := s.SetEncodedObject(treeObj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("removeEntry: store tree: %w", err)
	}
	return hash, nil
}
