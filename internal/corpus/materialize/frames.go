package materialize

import "knomit/internal/domain/fact"

// ancestors returns the starting frame's ancestors in reading order.
//
// User ruling (2026-10-28, round 3): the frame you are at is the CHILD and
// every frame it refs is a PARENT. Each ref is read at the commit its
// DERIVED_FROM edge pins (or at the latest version under unpin), so a cited
// version precedes, or shares the commit of, the citing one: walking parents
// moves backward through the temporal graph on its own. Nothing here
// computes when a frame was created.
//
// The walk is breadth-first in refs order from the starting frame, over any
// ref whose resolved target is a frame, in this repo or (with cross_kb and
// the caller's reach) another one: resolve() decides that exactly as for any
// other fact. The "each path written once" rule is what ends it: a frame
// already in the set is not added again, so there is no cycle detection.
// The first-reached order is the cap order (closest first, so a cut never
// drops a parent before a grandparent) and the tie-break of the order.
// Ancestors are included whatever their distance, bounded only by
// MaxNodes, and are terminal: none of their other refs is ever expanded.
func (w *walker) ancestors(start *node) ([]*node, error) {
	found := []*node{}
	disc := map[key]int{start.key: 0}
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
			if _, seen := w.nodes[r.target]; !seen {
				a := w.adopt(r, f.dist+1)
				a.ancestor = true
				if !a.included {
					// The cap is reached: it is recorded as cut, so the frame
					// citing it lists it as not included (size).
					continue
				}
				disc[a.key] = len(found) + 1
				found = append(found, a)
				queue = append(queue, a)
			}
			// A parent edge orders f after r's target only when the target
			// was reached AFTER f. Two frames that ref each other (at one
			// commit, or one through a stale pin) would otherwise each be the
			// other's parent: the one reached first from the starting frame
			// is the child, by the ruling, and its partner's ref back to it
			// is a back-reference only.
			if d, ok := disc[r.target]; ok && d > disc[f.key] {
				f.parents = append(f.parents, r.target)
			}
		}
	}
	return topoSort(found), nil
}

// topoSort orders ancestors so each comes before every frame that cites it
// as a parent, a shared ancestor once, ties by first-reached order (Kahn's
// algorithm with the ready set ordered by discovery). Every parent edge
// points to a frame reached later than its child (see ancestors), so the
// edges cannot form a cycle and a ready ancestor always exists.
func topoSort(found []*node) []*node {
	idx := make(map[key]int, len(found))
	for i, a := range found {
		idx[a.key] = i
	}
	emitted := make([]bool, len(found))
	ready := func(i int) bool {
		for _, p := range found[i].parents {
			if j, ok := idx[p]; ok && !emitted[j] {
				return false
			}
		}
		return true
	}
	out := make([]*node, 0, len(found))
	for len(out) < len(found) {
		for i := range found {
			if !emitted[i] && ready(i) {
				emitted[i] = true
				out = append(out, found[i])
				break
			}
		}
	}
	return out
}
