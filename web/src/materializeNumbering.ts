// Section numbers for the materialized article, read from the walk's TREE.
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
// Numbers are then the outline kind: the root is the unnumbered top, its
// children are 1, 2, 3, ..., their children 1.1, 1.2, 2.1, ... A fact reached
// through several parents is written once, so it has one number (its first
// visit); the other parents' builds_on links point at that number.
//
// FRAMES: the ancestors come first as an unnumbered preamble, then the frame
// itself as the unnumbered top, and the numbers start at the frame's children.
//
// Numbers never change as pages arrive: a node's parent is always an earlier
// node, and later nodes only add later children.

import type { MatNode } from './materializeApi';

/** n -> number text; '' means unnumbered (the top, or a frame's ancestor). */
export function numberSections(nodes: MatNode[]): Map<number, string> {
  const sorted = [...nodes].sort((a, b) => a.n - b.n);
  const top = sorted.find(x => x.role === 'root' || x.role === 'focal');
  const numbers = new Map<number, string>();
  const children = new Map<number, number>();
  sorted.forEach((t, i) => {
    if (t.role === 'ancestor' || t.role === 'root' || t.role === 'focal') {
      numbers.set(t.n, '');
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
    const base = parent ? numbers.get(parent.n) ?? '' : '';
    numbers.set(t.n, base ? `${base}.${k}` : String(k));
  });
  return numbers;
}
