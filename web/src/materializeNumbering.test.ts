import { describe, expect, it } from 'vitest';
import type { MatNode } from './materializeApi';
import { numberSections } from './materializeNumbering';

// A node written at ordinal n that cites `cites` (ordinals in the document).
function node(n: number, role: string, cites: number[] = []): MatNode {
  return {
    n, path: `kb/x/${n}.md`, repo: 'r', commit: 'c', depth: 0, role, title: `T${n}`, type: 'observation', kind: 'epistemic',
    confidence: 0.9, anchor_id: `f-${n}`,
    builds_on: cites.map(c => ({ n: c, title: `T${c}`, id: String(c), anchor_id: `f-${c}` })),
  };
}
const nums = (nodes: MatNode[]) => Object.fromEntries([...numberSections(nodes)].map(([n, s]) => [n, s]));

describe('section numbers', () => {
  it('numbers a nested walk like an outline: the root is the unnumbered top, children 1, 2, grandchildren 1.1, 2.1', () => {
    // root(1) cites A(2) and D(5); A cites B(3) and C(4); D cites E(6)
    const walk = [node(1, 'root', [2, 5]), node(2, 'ref', [3, 4]), node(3, 'ref'), node(4, 'ref'), node(5, 'ref', [6]), node(6, 'ref')];
    expect(nums(walk)).toEqual({ 1: '', 2: '1', 3: '1.1', 4: '1.2', 5: '2', 6: '2.1' });
  });

  it('numbers three levels deep (1.2.1)', () => {
    const walk = [node(1, 'root', [2]), node(2, 'ref', [3, 4]), node(3, 'ref'), node(4, 'ref', [5]), node(5, 'ref')];
    expect(nums(walk)).toMatchObject({ 2: '1', 3: '1.1', 4: '1.2', 5: '1.2.1' });
  });

  it('a fact reached through several parents is written once, numbered at its FIRST visit, and later parents link to that number', () => {
    // root cites A(2) and B(4); A cites S(3); B also cites S. S was first reached under A.
    const walk = [node(1, 'root', [2, 4]), node(2, 'ref', [3]), node(3, 'ref'), node(4, 'ref', [3])];
    const n = nums(walk);
    expect(n).toEqual({ 1: '', 2: '1', 3: '1.1', 4: '2' });
    // B's builds_on link targets ordinal 3, which reads as section 1.1
    expect(n[walk[3].builds_on![0].n]).toBe('1.1');
  });

  it('a fact the root cites directly AFTER a subtree that also cites it is a child of the subtree, not of the root', () => {
    // root cites A(2) then S; A cites S: S is reached inside A first
    const walk = [node(1, 'root', [2, 3]), node(2, 'ref', [3]), node(3, 'ref')];
    expect(nums(walk)).toEqual({ 1: '', 2: '1', 3: '1.1' });
  });

  it('frames: ancestors are an unnumbered preamble, the frame is the unnumbered top, numbers start at the frame\'s children', () => {
    const walk = [node(1, 'ancestor'), node(2, 'ancestor', [1]), node(3, 'focal', [2, 4, 6]), node(4, 'ref', [5]), node(5, 'ref'), node(6, 'ref')];
    expect(nums(walk)).toEqual({ 1: '', 2: '', 3: '', 4: '1', 5: '1.1', 6: '2' });
  });

  it('a number does not change when later pages arrive', () => {
    const walk = [node(1, 'root', [2, 4]), node(2, 'ref', [3]), node(3, 'ref'), node(4, 'ref', [5]), node(5, 'ref')];
    const early = nums(walk.slice(0, 3));
    const late = nums(walk);
    for (const [k, v] of Object.entries(early)) expect(late[Number(k)]).toBe(v);
  });
});
