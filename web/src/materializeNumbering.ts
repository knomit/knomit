// The walk TREE, recovered on the client, and the section numbers read from it.
//
// The walk writes facts in a pre-order depth-first order and writes a fact the
// first time it reaches it, so the order is a tree walk with every repeat
// folded away. The tree's edges are not on the wire, but they are recoverable:
// a fact t is written by the fact that first reaches it, and that is the
// LATEST-written fact before t that cites t (a fact that finished before t
// would already have written it; one still being walked is on the stack, and
// the deepest of those is the latest). builds_on lists exactly the facts a
// fact cites that are in the document, so:
//
//   parent(t) = the latest earlier, non-ancestor node whose builds_on has t.n.
//
// THE PAYLOAD'S `depth` IS NOT THIS TREE'S DEPTH. It is the BFS distance from
// the root (what decides which facts are included). A real walk with
// depth <= 6 can nest 35 levels deep in reading order, because the order is a
// depth-first walk over a breadth-first set. Anything about NESTING (heading
// ladder, gutter, numbers, the section path) must use the tree depth below.
//
// Numbers are the outline kind: the root is the unnumbered top, its children
// are 1, 2, 3, ..., their children 1.1, 1.2, 2.1, ... A fact reached through
// several parents is written once, so it has one number (its first visit).
//
// FRAMES: the ancestors come first as an unnumbered preamble, then the frame
// itself as the unnumbered top, and the numbers start at the frame's children.
//
// Nothing here changes as pages arrive: a node's parent is always an earlier
// node, and later nodes only add later children.

import type { MatNode } from './materializeApi';

export interface TreeInfo {
  /** The full outline number ('' for the top and a frame's ancestors). */
  num: string;
  parts: number[];
  /** Tree depth: 0 for the top and ancestors, 1 for its children, ... */
  depth: number;
  /** The parent's n, null for the top and ancestors. */
  parent: number | null;
}

/** n -> its place in the walk tree. */
export function treeInfo(nodes: MatNode[]): Map<number, TreeInfo> {
  const sorted = [...nodes].sort((a, b) => a.n - b.n);
  const top = sorted.find(x => x.role === 'root' || x.role === 'focal');
  const info = new Map<number, TreeInfo>();
  const children = new Map<number, number>();
  sorted.forEach((t, i) => {
    if (t.role === 'ancestor' || t.role === 'root' || t.role === 'focal') {
      info.set(t.n, { num: '', parts: [], depth: 0, parent: null });
      return;
    }
    let parent: MatNode | undefined;
    for (let j = i - 1; j >= 0; j--) {
      const c = sorted[j];
      if (c.role !== 'ancestor' && c.builds_on?.some(l => l.n === t.n)) { parent = c; break; }
    }
    parent ??= top;
    const key = parent?.n ?? -1;
    const k = (children.get(key) ?? 0) + 1;
    children.set(key, k);
    const pi = parent ? info.get(parent.n) : undefined;
    const parts = [...(pi?.parts ?? []), k];
    info.set(t.n, { num: parts.join('.'), parts, depth: (pi?.depth ?? 0) + 1, parent: parent?.n ?? null });
  });
  return info;
}

/** n -> number text; '' means unnumbered (the top, or a frame's ancestor). */
export function numberSections(nodes: MatNode[]): Map<number, string> {
  return new Map([...treeInfo(nodes)].map(([n, t]) => [n, t.num]));
}
