package store

import (
	"fmt"
	"knomit/internal/pki"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// walkHistory visits every commit reachable from start and not from stop,
// parents before children (a depth-first post-order), once each. A start in
// stop is reachable from stop, so nothing is visited: E4 must not judge an
// agent tip that is already on origin's main (a forge merge it fast-forwarded
// onto).
func walkHistory(st storer.EncodedObjectStorer, start plumbing.Hash, stop map[plumbing.Hash]bool, visit func(*object.Commit)) error {
	if stop[start] {
		return nil
	}
	seen := map[plumbing.Hash]bool{}
	type frame struct {
		c    *object.Commit
		next int
	}
	root, err := object.GetCommit(st, start)
	if err != nil {
		return fmt.Errorf("verify: commit %s: %w", start, err)
	}
	stack := []*frame{{c: root}}
	seen[start] = true
	for len(stack) > 0 {
		f := stack[len(stack)-1]
		if f.next < len(f.c.ParentHashes) {
			p := f.c.ParentHashes[f.next]
			f.next++
			if seen[p] || stop[p] {
				continue
			}
			seen[p] = true
			pc, err := object.GetCommit(st, p)
			if err != nil {
				return fmt.Errorf("verify: commit %s: %w", p, err)
			}
			stack = append(stack, &frame{c: pc})
			continue
		}
		stack = stack[:len(stack)-1]
		visit(f.c)
	}
	return nil
}

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

// agentIDOfAuthor returns the agent id in an agent author address,
// <id>[+<op>]@agents.knomit.io, or "" for any other address. It is
// pki.AgentIDOfEmail, the ONE string operation on an identity, shared with the
// certificate's email SAN so the two can never disagree (R2). The id is
// OPAQUE: it is looked up and compared, never parsed further.
func agentIDOfAuthor(email string) string { return pki.AgentIDOfEmail(email) }

// parentTrees returns the trees of c's parents, or the empty tree (nil) for a
// root commit.
func parentTrees(c *object.Commit) ([]*object.Tree, error) {
	if c.NumParents() == 0 {
		return []*object.Tree{nil}, nil
	}
	out := make([]*object.Tree, 0, c.NumParents())
	for i := range c.NumParents() {
		p, err := c.Parent(i)
		if err != nil {
			return nil, err
		}
		pt, err := p.Tree()
		if err != nil {
			return nil, err
		}
		out = append(out, pt)
	}
	return out, nil
}

// shortRefHash returns the first 8 chars of a ref hash for log output, or
// "<zero>" for the zero hash.
func shortRefHash(h plumbing.Hash) string {
	if h == plumbing.ZeroHash {
		return "<zero>"
	}
	return h.String()[:8]
}
