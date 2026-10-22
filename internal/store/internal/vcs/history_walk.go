package vcs

import (
	"context"
	"fmt"
	"strings"

	"knomit/internal/foundation/pki"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// WalkHistory visits every commit reachable from start and not from stop,
// parents before children (a depth-first post-order), once each. A start in
// stop is reachable from stop, so nothing is visited: E4 must not judge an
// agent tip that is already on origin's main (a forge merge it fast-forwarded
// onto).
func WalkHistory(st storer.EncodedObjectStorer, start plumbing.Hash, stop map[plumbing.Hash]bool, visit func(*object.Commit)) error {
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

// CheckTreeShape walks tree recursively by its entries, NOT with go-git's
// TreeWalker, and fails on the first entry that is not well formed:
//   - a directory entry (mode 040000) whose object is missing or is not a tree;
//   - a file or symlink entry whose object is missing or is not a blob;
//   - a name go-git refuses to read back (TreeEntryFile / FindEntry, its
//     ValidTreePath), a name holding '/', or a name twice in one tree;
//   - a tree deeper than MaxShapeDepth.
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
func CheckTreeShape(tree *object.Tree, ok map[plumbing.Hash]bool) error {
	return treeShape(tree, "", 0, ok)
}

// MaxShapeDepth bounds the recursion as go-git's own walker does (its
// maxTreeDepth).
const MaxShapeDepth = 1024

func treeShape(t *object.Tree, dir string, depth int, ok map[plumbing.Hash]bool) error {
	if ok[t.Hash] {
		return nil
	}
	if depth > MaxShapeDepth {
		return fmt.Errorf("%s: deeper than %d trees", dir, MaxShapeDepth)
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

// AgentIDOfAuthor returns the agent id in an agent author address,
// <id>[+<op>]@agents.knomit.io, or "" for any other address. It is
// pki.AgentIDOfEmail, the ONE string operation on an identity, shared with the
// certificate's email SAN so the two can never disagree (R2). The id is
// OPAQUE: it is looked up and compared, never parsed further.
func AgentIDOfAuthor(email string) string { return pki.AgentIDOfEmail(email) }

// ParentTrees returns the trees of c's parents, or the empty tree (nil) for a
// root commit.
func ParentTrees(c *object.Commit) ([]*object.Tree, error) {
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

// ShortRefHash returns the first 8 chars of a ref hash for log output, or
// "<zero>" for the zero hash.
func ShortRefHash(h plumbing.Hash) string {
	if h == plumbing.ZeroHash {
		return "<zero>"
	}
	return h.String()[:8]
}

// HasSharedHistory checks whether localBranch of local shares any commits with
// remoteBranch of remote. Uses a bounded walk (max 1000 commits) to avoid
// scanning huge histories.
func HasSharedHistory(ctx context.Context, local *Repo, localBranch string, remote *Repo, remoteBranch string) (bool, error) {
	const maxCommits = 1000

	localHash, err := local.ResolveRef(ctx, localBranch)
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: local ref: %w", err)
	}
	localHashes := make(map[plumbing.Hash]struct{})
	localIter, err := local.Repository().Log(&gogit.LogOptions{From: localHash})
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: local log: %w", err)
	}
	count := 0
	if err := localIter.ForEach(func(c *object.Commit) error {
		if count >= maxCommits {
			return storer.ErrStop
		}
		localHashes[c.Hash] = struct{}{}
		count++
		return nil
	}); err != nil {
		return false, fmt.Errorf("HasSharedHistory: local walk: %w", err)
	}

	remoteHash, err := remote.ResolveRef(ctx, remoteBranch)
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: remote ref: %w", err)
	}
	remoteIter, err := remote.Repository().Log(&gogit.LogOptions{From: remoteHash})
	if err != nil {
		return false, fmt.Errorf("HasSharedHistory: remote log: %w", err)
	}
	count = 0
	found := false
	if err := remoteIter.ForEach(func(c *object.Commit) error {
		if count >= maxCommits {
			return storer.ErrStop
		}
		if _, ok := localHashes[c.Hash]; ok {
			found = true
			return storer.ErrStop
		}
		count++
		return nil
	}); err != nil && !found {
		return false, fmt.Errorf("HasSharedHistory: remote walk: %w", err)
	}
	return found, nil
}
