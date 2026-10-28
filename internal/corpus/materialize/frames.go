package materialize

import "knomit/internal/domain/fact"

// ancestors returns the starting frame's ancestors in reading order: every
// parent before the frames that cite it, the top of the stack first.
//
// User ruling (2026-10-28): the frame you are at is the CHILD and every frame
// it refs is a PARENT. Each ref is read at the commit its DERIVED_FROM edge
// pins (or at the latest version under unpin), so walking parents moves
// backward through the temporal graph on its own; nothing computes when a
// frame was created. A frame in another repo is a parent like any other ref
// (resolve() applies cross_kb and the caller's reach), and the walk continues
// inside that repo.
//
// Two passes over the same frame refs, in refs order:
//   - INCLUSION, breadth-first from the starting frame: closest first against
//     MaxNodes, so a cut never drops a parent before a grandparent. Ancestors
//     are included whatever their distance and are terminal: none of their
//     other refs is ever expanded.
//   - ORDER, a depth-first POST-ORDER from the starting frame: walk back from
//     the frame to the top, then write top-down. A frame is emitted after all
//     the parents it reaches, so every parent precedes its child. A ref to a
//     frame already visited (the starting frame included) is a
//     back-reference: that is how a mutual pair settles, the frame you are at
//     being the child, and why the walk cannot loop. No cycle detection.
func (w *walker) ancestors(start *node) ([]*node, error) {
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
			if a.included {
				queue = append(queue, a)
			}
			// Otherwise the cap is reached: it is recorded as cut, so the
			// frame citing it lists it as not included (size).
		}
	}

	var out []*node
	visited := map[key]bool{start.key: true}
	var visit func(f *node)
	visit = func(f *node) {
		for _, r := range f.refs {
			if r.kind != refNode || visited[r.target] {
				continue
			}
			a := w.nodes[r.target]
			if a == nil || !a.ancestor || !a.included {
				continue
			}
			visited[a.key] = true
			visit(a)
			out = append(out, a)
		}
	}
	visit(start)
	return out, nil
}
