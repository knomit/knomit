// Low-level git plumbing: blob creation, tree building/modification, and commit
// creation. These helpers are used by the higher-level read/write/sync operations.
//
// All functions are unexported and take *storegit.Storer parameters.
package vcs

import (
	_ "embed"
	"fmt"
	"io"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"golang.org/x/crypto/ssh"

	storegit "knomit/internal/engine/store/git"
	"knomit/internal/engine/store/model"
)

// WriteBlobToStore stores content as a loose blob and returns its hash.
//
// Split out of WriteFileToStore because a merge resolution needs the BLOB
// only: the tree is being assembled entry by entry by the merge itself, and
// there is no commit until every path has been decided.
func WriteBlobToStore(s *storegit.Storer, content []byte) (plumbing.Hash, error) {
	obj := s.NewEncodedObject()
	obj.SetType(plumbing.BlobObject)
	w, err := obj.Writer()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("writeBlobToStore: blob writer: %w", err)
	}
	if _, err := w.Write(content); err != nil {
		w.Close()
		return plumbing.ZeroHash, fmt.Errorf("writeBlobToStore: blob write: %w", err)
	}
	w.Close()
	hash, err := s.SetEncodedObject(obj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("writeBlobToStore: store blob: %w", err)
	}
	return hash, nil
}

// WriteFileToStore creates a blob+tree+commit for path/content.
// parentCommitHash is ZeroHash for the initial commit (no parent).
// signer signs the commit before it is stored (StoreCommit); nil stores it
// unsigned, which only the init commits of a new repository do.
// Returns (commitHash, blobHash, error).
func WriteFileToStore(s *storegit.Storer, signer ssh.Signer, parentCommitHash plumbing.Hash, path, content, message string, author, committer object.Signature) (plumbing.Hash, plumbing.Hash, error) {
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
	newRootHash, err := BuildTree(s, existingTree, path, blobHash)
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

	commitHash, err := StoreCommit(s, signer, commit)
	if err != nil {
		return plumbing.ZeroHash, plumbing.ZeroHash, fmt.Errorf("writeFileToStore: %w", err)
	}

	return commitHash, blobHash, nil
}

// BuildTree constructs a new root tree by adding/replacing path (which may be
// nested, e.g. "general/technology/go/abc123.md") with blobHash. existing may be nil for an
// empty tree. The function recurses through path segments, creating or updating
// subtrees as needed.
func BuildTree(s *storegit.Storer, existing *object.Tree, path string, blobHash plumbing.Hash) (plumbing.Hash, error) {
	return BuildTreeMode(s, existing, path, filemode.Regular, blobHash)
}

// BuildTreeMode is BuildTree with the leaf entry's mode chosen by the caller.
func BuildTreeMode(s *storegit.Storer, existing *object.Tree, path string, mode filemode.FileMode, blobHash plumbing.Hash) (plumbing.Hash, error) {
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]

	if len(parts) == 1 {
		// Leaf: insert/replace the file entry in this tree.
		return UpsertEntry(s, existing, object.TreeEntry{
			Name: name,
			Mode: mode,
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

	subHash, err := BuildTreeMode(s, subtree, rest, mode, blobHash)
	if err != nil {
		return plumbing.ZeroHash, err
	}

	return UpsertEntry(s, existing, object.TreeEntry{
		Name: name,
		Mode: filemode.Dir,
		Hash: subHash,
	})
}

// UpsertEntry adds or replaces entry in a copy of existing (nil means empty tree),
// encodes the resulting tree, stores it, and returns its hash.
func UpsertEntry(s *storegit.Storer, existing *object.Tree, entry object.TreeEntry) (plumbing.Hash, error) {
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

// DeleteFileFromStore creates a commit that removes path from the tree rooted
// at parentCommitHash, signed by signer before it is stored.
func DeleteFileFromStore(s *storegit.Storer, signer ssh.Signer, parentCommitHash plumbing.Hash, path, message string, author, committer object.Signature) (plumbing.Hash, error) {
	parentCommit, err := object.GetCommit(s, parentCommitHash)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: get parent commit: %w", err)
	}
	existingTree, err := parentCommit.Tree()
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: get parent tree: %w", err)
	}

	newRootHash, err := DeleteFromTree(s, existingTree, path)
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

	commitHash, err := StoreCommit(s, signer, commit)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("deleteFileFromStore: %w", err)
	}
	return commitHash, nil
}

// DeleteFromTree removes path from the tree rooted at existing, recursing
// through directory segments as needed, and returns the new root's hash. It
// writes trees the way git does (#375):
//
//   - A folder left with no entries is dropped from its parent, never kept as
//     an empty tree. This cascades: emptying kb/x/y also drops kb/x when y was
//     its only entry. The root itself is never dropped; an empty root tree is
//     legal git.
//   - A path whose folder does not exist is a no-op, exactly like a missing
//     leaf in an existing folder: nothing is there, so there is nothing to
//     delete, and the returned hash is existing's own.
//
// Callers that need "the path must exist" check it themselves (DeleteFact's
// existence check, BatchWriteFactsMustExist); a storage failure reading a
// subtree is still an error.
//
// A dropped folder's empty tree is never stored: it would be unreachable the
// moment the parent drops its entry, and Verify reports such orphans.
func DeleteFromTree(s *storegit.Storer, existing *object.Tree, path string) (plumbing.Hash, error) {
	hash, empty, err := deletePath(s, existing, path)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if empty {
		return storeTree(s, nil)
	}
	return hash, nil
}

// deletePath is DeleteFromTree's recursion. empty reports that the tree at
// this level would have no entries left; it is then NOT stored (hash is zero),
// so the caller can drop the entry without leaving an orphan behind.
func deletePath(s *storegit.Storer, existing *object.Tree, path string) (hash plumbing.Hash, empty bool, err error) {
	parts := strings.SplitN(path, "/", 2)
	name := parts[0]

	if len(parts) == 1 {
		// Leaf: remove the entry from this tree.
		return storeUnlessEmpty(s, removeEntry(existing, name))
	}

	// Recurse into subtree.
	rest := parts[1]
	var subtree *object.Tree
	for _, e := range existing.Entries {
		if e.Name == name && e.Mode == filemode.Dir {
			subtree, err = object.GetTree(s, e.Hash)
			if err != nil {
				return plumbing.ZeroHash, false, fmt.Errorf("deleteFromTree: get subtree %q: %w", name, err)
			}
			break
		}
	}
	if subtree == nil {
		return existing.Hash, len(existing.Entries) == 0, nil
	}

	subHash, subEmpty, err := deletePath(s, subtree, rest)
	if err != nil {
		return plumbing.ZeroHash, false, err
	}
	if subEmpty {
		return storeUnlessEmpty(s, removeEntry(existing, name))
	}
	hash, err = UpsertEntry(s, existing, object.TreeEntry{
		Name: name,
		Mode: filemode.Dir,
		Hash: subHash,
	})
	return hash, false, err
}

// removeEntry returns a copy of existing's entries minus the one named name.
func removeEntry(existing *object.Tree, name string) []object.TreeEntry {
	var entries []object.TreeEntry
	if existing != nil {
		for _, e := range existing.Entries {
			if e.Name != name {
				entries = append(entries, e)
			}
		}
	}
	return entries
}

// storeUnlessEmpty stores entries as a tree, or reports empty without storing
// anything when there are none.
func storeUnlessEmpty(s *storegit.Storer, entries []object.TreeEntry) (plumbing.Hash, bool, error) {
	if len(entries) == 0 {
		return plumbing.ZeroHash, true, nil
	}
	h, err := storeTree(s, entries)
	return h, false, err
}

// storeTree sorts entries, encodes them as a tree, stores it and returns its
// hash. No entries stores the empty tree.
func storeTree(s *storegit.Storer, entries []object.TreeEntry) (plumbing.Hash, error) {
	sort.Sort(object.TreeEntrySorter(entries))
	tree := &object.Tree{Entries: entries}
	treeObj := s.NewEncodedObject()
	if err := tree.Encode(treeObj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("storeTree: encode tree: %w", err)
	}
	hash, err := s.SetEncodedObject(treeObj)
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("storeTree: store tree: %w", err)
	}
	return hash, nil
}

// treeFileInsensitive walks a git tree matching each path component
// case-insensitively and returns the file contents.
func treeFileInsensitive(repo *gogit.Repository, tree *object.Tree, path string) (string, error) {
	entry, err := TreeEntryInsensitive(repo, tree, path)
	if err != nil {
		return "", err
	}
	blob, err := repo.BlobObject(entry.Hash)
	if err != nil {
		return "", err
	}
	r, err := blob.Reader()
	if err != nil {
		return "", err
	}
	defer r.Close()
	b, err := io.ReadAll(r)
	return string(b), err
}

// treeEntryInsensitiveVia is TreeEntryInsensitive with the subtree loader
// supplied, so a caller walking many paths can cache trees.
func treeEntryInsensitiveVia(load func(plumbing.Hash) (*object.Tree, error), tree *object.Tree, path string) (*object.TreeEntry, error) {
	parts := strings.Split(path, "/")
	cur := tree
	for i, part := range parts {
		var matched *object.TreeEntry
		for j := range cur.Entries {
			if strings.EqualFold(cur.Entries[j].Name, part) {
				matched = &cur.Entries[j]
				break
			}
		}
		if matched == nil {
			return nil, fmt.Errorf("component %q: %w", part, model.ErrPathNotFound)
		}
		if i == len(parts)-1 {
			return matched, nil
		}
		if matched.Mode != filemode.Dir {
			// A file where the path expects a directory: the path is absent.
			return nil, fmt.Errorf("component %q is not a directory: %w", part, model.ErrPathNotFound)
		}
		sub, err := load(matched.Hash)
		if err != nil {
			return nil, fmt.Errorf("subtree %q: %w", part, err)
		}
		cur = sub
	}
	return nil, fmt.Errorf("empty path")
}

// TreeEntryInsensitive walks a git tree matching each path component
// case-insensitively and returns the entry at path. A missing component is
// ErrPathNotFound.
func TreeEntryInsensitive(repo *gogit.Repository, tree *object.Tree, path string) (*object.TreeEntry, error) {
	return treeEntryInsensitiveVia(repo.TreeObject, tree, path)
}

// BlobObjectType is the go-git integer for plumbing.BlobObject.
const BlobObjectType = 3
