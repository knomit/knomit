package verify

import (
	"fmt"
	"io"
	"maps"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/object"

	"knomit/internal/engine/store/internal/vcs"
)

// M3Verdict is merge rule M3's answer for one merge commit: whether its tree
// is exactly the path-level three-way merge of its two parents over their one
// merge base, and if not, why.
//
// M3 is how an UNSIGNED merge (GitHub's auto-merge workflow) is accepted
// without a knomit signature: it may only combine what its parents already
// carry. For every path (every non-tree entry: files, symlinks and gitlinks,
// compared on hash and mode) with base blob b and parent blobs a and c, the result
// must be c when a==b, a when c==b, a when a==c, and anything else ("both
// sides changed") needs a signature. Absence is a value, so a deletion follows
// the same table. The rule that preceded it ("every path equals SOME parent")
// accepted merges that resurrect a retracted fact, revert an update, or drop a
// side wholesale (-s ours).
//
// Whether the PARENTS are acceptable is the fold's question, not M3's.
type M3Verdict struct {
	OK     bool
	Reason string
}

// pathEntry is one file in a flattened tree: its blob and its mode, both of
// which must match for two entries to be the same.
type pathEntry struct {
	Hash plumbing.Hash
	Mode filemode.FileMode
}

func CheckM3(merge *object.Commit) (M3Verdict, error) {
	if merge.NumParents() != 2 {
		return M3Verdict{Reason: fmt.Sprintf("%d parents: only a two-parent merge can be accepted without a signature", merge.NumParents())}, nil
	}
	p1, err := merge.Parent(0)
	if err != nil {
		return M3Verdict{}, fmt.Errorf("M3: parent 1: %w", err)
	}
	p2, err := merge.Parent(1)
	if err != nil {
		return M3Verdict{}, fmt.Errorf("M3: parent 2: %w", err)
	}
	bases, err := p1.MergeBase(p2)
	if err != nil {
		return M3Verdict{}, fmt.Errorf("M3: merge base: %w", err)
	}
	if len(bases) != 1 {
		return M3Verdict{Reason: fmt.Sprintf("%d merge bases (criss-cross or disjoint): a signature is required", len(bases))}, nil
	}
	// flat is every non-tree entry of c's tree: files, symlinks AND gitlinks
	// (submodule entries), each with its mode. tree.Files() would not do: it
	// skips submodule entries, so a "clean" merge could add a gitlink its
	// parents do not carry and still pass. The TreeWalker itself ends
	// silently at a directory entry that is not a tree, so the tree's shape
	// is checked first (CheckTreeShape): such an entry is an error, never
	// the end of the walk.
	flat := func(c *object.Commit) (map[string]pathEntry, error) {
		tree, err := c.Tree()
		if err != nil {
			return nil, err
		}
		if err := vcs.CheckTreeShape(tree, nil); err != nil {
			return nil, err
		}
		out := map[string]pathEntry{}
		w := object.NewTreeWalker(tree, true, nil)
		defer w.Close()
		for {
			name, e, err := w.Next()
			if err == io.EOF {
				return out, nil
			}
			if err != nil {
				return nil, err
			}
			if e.Mode == filemode.Dir {
				continue
			}
			out[name] = pathEntry{Hash: e.Hash, Mode: e.Mode}
		}
	}
	b, err := flat(bases[0])
	if err != nil {
		return M3Verdict{}, fmt.Errorf("M3: base tree: %w", err)
	}
	a, err := flat(p1)
	if err != nil {
		return M3Verdict{}, fmt.Errorf("M3: parent 1 tree: %w", err)
	}
	c, err := flat(p2)
	if err != nil {
		return M3Verdict{}, fmt.Errorf("M3: parent 2 tree: %w", err)
	}
	m, err := flat(merge)
	if err != nil {
		return M3Verdict{}, fmt.Errorf("M3: merge tree: %w", err)
	}

	paths := map[string]struct{}{}
	for _, t := range []map[string]pathEntry{b, a, c, m} {
		for p := range maps.Keys(t) {
			paths[p] = struct{}{}
		}
	}
	for p := range paths {
		bv, bok := b[p]
		av, aok := a[p]
		cv, cok := c[p]
		mv, mok := m[p]
		same := func(x pathEntry, xok bool, y pathEntry, yok bool) bool { return xok == yok && (!xok || x == y) }
		var want pathEntry
		var wok bool
		switch {
		case same(av, aok, bv, bok):
			want, wok = cv, cok
		case same(cv, cok, bv, bok):
			want, wok = av, aok
		case same(av, aok, cv, cok):
			want, wok = av, aok
		default:
			return M3Verdict{Reason: fmt.Sprintf("both sides changed %s: a signature is required", p)}, nil
		}
		if !same(mv, mok, want, wok) {
			return M3Verdict{Reason: fmt.Sprintf("the merge's %s is not the three-way result of its parents", p)}, nil
		}
	}
	return M3Verdict{OK: true}, nil
}
