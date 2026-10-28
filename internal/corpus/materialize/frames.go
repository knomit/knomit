package materialize

import (
	"math"

	"github.com/go-git/go-git/v5/plumbing"

	"knomit/internal/domain/fact"
)

// ancestors returns the starting frame's ancestors in reading order.
//
// A parent is a ref of a frame whose target is a frame (at the version the
// ref resolves to: the cited one, or the latest under unpin) AND that target
// was created before it (isParent); a frame may have several. The ancestor
// set is the upward closure from the starting frame over parent refs, walked
// breadth-first in refs order: that walk's first-reached order is both the
// cap order (closest first, so a cut never drops a parent before a
// grandparent) and the tie-break of the topological sort. Ancestors are
// included whatever their distance, bounded only by MaxNodes, and are
// terminal: none of their other refs is ever expanded.
func (w *walker) ancestors(start *node) ([]*node, error) {
	var found []*node
	queue := []*node{start}
	for len(queue) > 0 {
		f := queue[0]
		queue = queue[1:]
		refs, err := w.resolve(f)
		if err != nil {
			return nil, err
		}
		for _, r := range refs {
			if r.kind != refNode || r.f.Type != fact.Frame {
				continue
			}
			parent, err := w.isParent(f, r)
			if err != nil {
				return nil, err
			}
			if !parent {
				continue
			}
			f.parents = append(f.parents, r.target)
			if _, seen := w.nodes[r.target]; seen {
				continue
			}
			a := w.adopt(r, f.dist+1)
			a.ancestor = true
			if !a.included {
				// The cap is reached: it is recorded as cut, so the frame
				// citing it lists it as not included (size).
				continue
			}
			found = append(found, a)
			queue = append(queue, a)
		}
	}
	return topoSort(found), nil
}

// isParent decides whether frame f's ref r to another frame is a PARENT
// edge. User ruling (2026-10-28): links point backwards, like a stack; a
// parent never refs its child, and a parent had to exist before its child.
// So the target is a parent only when it was CREATED before f, decided by
// git ancestry of the two creation commits, never by commit timestamps
// (first-parent-vs-committed-at): the target's creation commit must be a
// strict ancestor of f's. Two frames that ref each other therefore resolve
// to one parent (the one created first), and two created on concurrent
// branches (neither commit an ancestor of the other) are not parent and
// child at all.
//
// Two cases cannot be ordered by ancestry and keep the ref as written:
// frames created in the SAME commit (one learn call may write a parent and
// its child), and a ref into another repo (two repos share no history),
// except that two such frames that ref each other are neither's parent.
// A frame whose creation commit the index does not hold is treated the same
// way.
func (w *walker) isParent(f *node, r resolved) (bool, error) {
	if f.m.id12 != r.m.id12 {
		return !refsBack(r, f), nil
	}
	cf, err := w.created(f.m, f.rel, f.commit)
	if err != nil {
		return false, err
	}
	ct, err := w.created(r.m, r.target.rel, r.commit)
	if err != nil {
		return false, err
	}
	if cf == "" || ct == "" || cf == ct {
		return true, nil
	}
	return f.m.svc.FactsRepo().IsAncestor(w.ctx, plumbing.NewHash(ct), plumbing.NewHash(cf))
}

// refsBack reports whether r's target cites f in its resolved version.
func refsBack(r resolved, f *node) bool {
	for _, raw := range r.f.Refs {
		c := fact.ClassifyRef(raw, r.m.id12)
		if c.Kind != fact.RefLocalFact && c.Kind != fact.RefForeignFact {
			continue
		}
		k := key{c.RepoID, c.Path}
		if c.Kind == fact.RefLocalFact {
			k.repo = r.m.id12
		}
		if k == f.key {
			return true
		}
	}
	return false
}

// created is the commit that created path as seen from the version at
// commit: the oldest add or modify on that version's first-parent line (a
// frame written on another branch and merged is created, on this line, by
// the merge). "" when the index holds no row for it.
func (w *walker) created(m *mount, rel, commit string) (string, error) {
	k := edgeKey{m.id12, rel, commit}
	if c, ok := w.creation[k]; ok {
		return c, nil
	}
	revs, err := m.svc.HistoryQuery().RevisionsBefore(w.ctx, m.branch, rel, commit, math.MaxInt32)
	if err != nil {
		return "", err
	}
	c := ""
	if len(revs) > 0 {
		c = revs[len(revs)-1].Commit
	}
	w.creation[k] = c
	return c, nil
}

// topoSort orders ancestors so each comes before every frame that cites it
// as a parent, a shared ancestor once. Among the ready ones the
// earliest-found goes first (Kahn's algorithm with a ready set ordered by
// discovery).
//
// Parents point to strictly older frames, so a cycle can only form among
// frames created in the same commit (or across repos). When no ancestor is
// ready, the earliest-found frame of a TOP cycle is taken: one whose every
// pending parent lies on a cycle with it. A frame merely below a cycle is
// never taken that way, so no frame is ever written above a parent it does
// not share a cycle with; the edge closing the cycle becomes an ordinary
// back-reference.
func topoSort(found []*node) []*node {
	idx := make(map[key]int, len(found))
	for i, a := range found {
		idx[a.key] = i
	}
	// parents[i]: the found ancestors that ancestor i cites as a parent.
	parents := make([][]int, len(found))
	for i, a := range found {
		for _, p := range a.parents {
			if j, ok := idx[p]; ok && j != i {
				parents[i] = append(parents[i], j)
			}
		}
	}
	emitted := make([]bool, len(found))
	pending := func(i int) []int {
		var out []int
		for _, p := range parents[i] {
			if !emitted[p] {
				out = append(out, p)
			}
		}
		return out
	}
	// reaches reports whether from reaches to over pending parent edges.
	reaches := func(from, to int) bool {
		seen := map[int]bool{from: true}
		stack := []int{from}
		for len(stack) > 0 {
			n := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, p := range pending(n) {
				if p == to {
					return true
				}
				if !seen[p] {
					seen[p] = true
					stack = append(stack, p)
				}
			}
		}
		return false
	}
	out := make([]*node, 0, len(found))
	for len(out) < len(found) {
		pick := -1
		for i := range found {
			if !emitted[i] && len(pending(i)) == 0 {
				pick = i
				break
			}
		}
		if pick < 0 {
			for i := range found {
				if emitted[i] {
					continue
				}
				top := true
				for _, p := range pending(i) {
					if !reaches(p, i) {
						top = false
						break
					}
				}
				if top {
					pick = i
					break
				}
			}
		}
		emitted[pick] = true
		out = append(out, found[pick])
	}
	return out
}
