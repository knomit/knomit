package store

import (
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// checkTreeShape walks tree recursively by its entries, NOT with go-git's
// TreeWalker, and fails on the first entry that is not well formed:
//   - a directory entry (mode 040000) whose object is missing or is not a tree;
//   - a file or symlink entry whose object is missing or is not a blob;
//   - a name go-git refuses to read back (TreeEntryFile / FindEntry, its
//     ValidTreePath), a name holding '/', or a name twice in one tree;
//   - a tree deeper than maxShapeDepth.
//
// A gitlink (submodule entry) names a commit in another repository and is not
// looked up, as everywhere else in the gate. Tree decoding canonicalises every
// mode (an unknown one reads as a gitlink), so there is no other mode.
//
// WHY: go-git v5.19.2's TreeWalker.Next (plumbing/object/tree.go) turns a
// failed GetTree on a directory entry into io.EOF, so every walk built on it
// (tree.Files(), NewTreeWalker, and object.DiffTree, whose tree noder lists
// children with a TreeWalker) ENDS SILENTLY at such an entry and never sees
// it or anything after it. A gate that judges such a tree with those walkers
// judges a prefix of it. Every walk the gate makes over a commit's tree is
// therefore preceded by this check, and fails instead of truncating.
//
// ok, when not nil, memoises the trees already found well formed: a tree is
// content-addressed, so one verdict holds for every commit that shares it.
// The error names the path of the offending entry.
func checkTreeShape(tree *object.Tree, ok map[plumbing.Hash]bool) error {
	return treeShape(tree, "", 0, ok)
}

// maxShapeDepth bounds the recursion as go-git's own walker does (its
// maxTreeDepth).
const maxShapeDepth = 1024

func treeShape(t *object.Tree, dir string, depth int, ok map[plumbing.Hash]bool) error {
	if ok[t.Hash] {
		return nil
	}
	if depth > maxShapeDepth {
		return fmt.Errorf("%s: deeper than %d trees", dir, maxShapeDepth)
	}
	seen := make(map[string]bool, len(t.Entries))
	for i := range t.Entries {
		e := &t.Entries[i]
		p := e.Name
		if dir != "" {
			p = dir + "/" + e.Name
		}
		if strings.Contains(e.Name, "/") {
			return fmt.Errorf("%s: an entry name holds '/'", p)
		}
		if seen[e.Name] {
			return fmt.Errorf("%s: the name appears twice in tree %s", p, t.Hash)
		}
		seen[e.Name] = true
		switch e.Mode {
		case filemode.Dir:
			// The name is unique in t (above), so the lookup by name resolves
			// THIS entry's object.
			sub, err := t.Tree(e.Name)
			if err != nil {
				return fmt.Errorf("%s (mode %s, %s) is not a tree: %w", p, e.Mode, e.Hash, err)
			}
			if err := treeShape(sub, p, depth+1, ok); err != nil {
				return err
			}
		case filemode.Submodule:
			if _, err := t.FindEntry(e.Name); err != nil {
				return fmt.Errorf("%s (gitlink %s): %w", p, e.Hash, err)
			}
		default: // regular, executable, symlink
			if _, err := t.TreeEntryFile(e); err != nil {
				return fmt.Errorf("%s (mode %s, %s) is not a blob: %w", p, e.Mode, e.Hash, err)
			}
		}
	}
	if ok != nil {
		ok[t.Hash] = true
	}
	return nil
}

// commitTreeShape is checkTreeShape over a commit's tree and every parent's
// tree: the gate's rules diff a commit against its parents (recordOwner,
// artifactRules) and M3 flattens both parents, and a parent tree that
// truncates truncates the diff.
//
// A malformed tree already on main wedges the gate: every later commit has it
// as a parent and is refused, the repair included, on every hosting and in
// every verify_signatures mode (the receive-pack closure checks parents too).
// The way out is a write that no gate judges. On a knomit host that is a
// host-local write to main (factIndex.deleteFile of the bogus entry's path:
// fileExists finds the entry by name and deletePath drops it whatever its
// mode). Only the immediate parents are checked, so commits on top of the
// repair are admitted again. On a forge it is a push to main past the
// required `knomit verify ci` check. Neither repairs commit_log:
// populateCommitLog cannot diff the malformed commit, so every later
// derivation on that branch still fails. A fresh repository is the clean
// recovery.
func commitTreeShape(c *object.Commit, ok map[plumbing.Hash]bool) error {
	tree, err := c.Tree()
	if err != nil {
		return fmt.Errorf("tree %s: %w", c.TreeHash, err)
	}
	if err := checkTreeShape(tree, ok); err != nil {
		return err
	}
	for i, ph := range c.ParentHashes {
		p, err := c.Parent(i)
		if err != nil {
			return fmt.Errorf("parent %s: %w", ph, err)
		}
		pt, err := p.Tree()
		if err != nil {
			return fmt.Errorf("parent %s: tree %s: %w", ph, p.TreeHash, err)
		}
		if err := checkTreeShape(pt, ok); err != nil {
			return fmt.Errorf("parent %s: %w", ph, err)
		}
	}
	return nil
}
