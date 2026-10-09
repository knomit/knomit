package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	storegit "knomit/internal/store/git"
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
//
// The trees are built in ONE pass (testingTree): each tree object the commit
// needs is written once. Applying the paths one at a time with buildTree, as
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
	root, err := testingTree(rh.gits, tree, files)
	if err != nil {
		return "", fmt.Errorf("TestingCommitFiles: %w", err)
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

// testingDir is one directory of a TestingCommitFiles batch: its files (name
// to content) and its subdirectories.
type testingDir struct {
	files map[string]string
	dirs  map[string]*testingDir
}

// testingTree writes the tree that results from adding files to existing (nil
// for an empty tree), writing each blob and each changed tree exactly once,
// and returns its hash. Entries of existing that the batch does not touch are
// kept; a batch path replaces an entry of the same name, as buildTree does
// (a file over a directory, or a directory over a file).
func testingTree(s *storegit.Storer, existing *object.Tree, files map[string]string) (plumbing.Hash, error) {
	top := &testingDir{}
	for p, content := range files {
		parts := strings.Split(p, "/")
		d := top
		for _, name := range parts[:len(parts)-1] {
			if _, clash := d.files[name]; clash {
				return plumbing.ZeroHash, fmt.Errorf("tree %q: %q is both a file and a directory in the batch", p, name)
			}
			if d.dirs == nil {
				d.dirs = map[string]*testingDir{}
			}
			if d.dirs[name] == nil {
				d.dirs[name] = &testingDir{}
			}
			d = d.dirs[name]
		}
		leaf := parts[len(parts)-1]
		if _, clash := d.dirs[leaf]; clash {
			return plumbing.ZeroHash, fmt.Errorf("tree %q: %q is both a file and a directory in the batch", p, leaf)
		}
		if d.files == nil {
			d.files = map[string]string{}
		}
		d.files[leaf] = content
	}
	return top.write(s, existing)
}

func (d *testingDir) write(s *storegit.Storer, existing *object.Tree) (plumbing.Hash, error) {
	var entries []object.TreeEntry
	if existing != nil {
		for _, e := range existing.Entries {
			_, f := d.files[e.Name]
			_, sub := d.dirs[e.Name]
			if !f && !sub {
				entries = append(entries, e)
			}
		}
	}
	for name, content := range d.files {
		h, err := writeBlobToStore(s, []byte(content))
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h})
	}
	for name, sub := range d.dirs {
		var prev *object.Tree
		if existing != nil {
			for _, e := range existing.Entries {
				if e.Name == name && e.Mode == filemode.Dir {
					t, err := object.GetTree(s, e.Hash)
					if err != nil {
						return plumbing.ZeroHash, fmt.Errorf("get subtree %q: %w", name, err)
					}
					prev = t
					break
				}
			}
		}
		h, err := sub.write(s, prev)
		if err != nil {
			return plumbing.ZeroHash, err
		}
		entries = append(entries, object.TreeEntry{Name: name, Mode: filemode.Dir, Hash: h})
	}
	sort.Sort(object.TreeEntrySorter(entries))
	obj := s.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(obj); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tree: %w", err)
	}
	return s.SetEncodedObject(obj)
}
