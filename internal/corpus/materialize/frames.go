package materialize

import "knomit/internal/domain/fact"

// ancestors returns the starting frame's ancestors in reading order.
//
// A parent is ANY ref of a frame whose target is a frame (at the version the
// ref resolves to: the cited one, or the latest under unpin); a frame may
// have several. The ancestor set is the upward closure from the starting
// frame over parent refs, walked breadth-first in refs order: that walk's
// first-reached order is both the cap order (closest first, so a cut never
// drops a parent before a grandparent) and the tie-break of the topological
// sort. Ancestors are included whatever their distance, bounded only by
// MaxNodes, and are terminal: none of their other refs is ever expanded.
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
	return topoSort(w, found), nil
}

// topoSort orders ancestors so each comes before every frame that cites it,
// a shared ancestor once. Among the ready ones the earliest-found goes first
// (Kahn's algorithm with a ready set ordered by discovery). A cycle among
// frames leaves no ready ancestor; the earliest-found remaining one is then
// taken, and the edge closing the cycle becomes an ordinary back-reference.
func topoSort(w *walker, found []*node) []*node {
	idx := make(map[key]int, len(found))
	for i, a := range found {
		idx[a.key] = i
	}
	// parents[i]: the found ancestors that ancestor i cites as a parent.
	parents := make([][]int, len(found))
	for i, a := range found {
		for _, r := range a.refs {
			if r.kind != refNode {
				continue
			}
			if j, ok := idx[r.target]; ok && j != i && w.nodes[r.target].f.Type == fact.Frame {
				parents[i] = append(parents[i], j)
			}
		}
	}
	emitted := make([]bool, len(found))
	out := make([]*node, 0, len(found))
	for len(out) < len(found) {
		pick := -1
		for i := range found {
			if emitted[i] {
				continue
			}
			ready := true
			for _, p := range parents[i] {
				if !emitted[p] {
					ready = false
					break
				}
			}
			if ready {
				pick = i
				break
			}
		}
		if pick < 0 {
			for i := range found {
				if !emitted[i] {
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
