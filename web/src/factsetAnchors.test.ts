import { describe, expect, it } from 'vitest';
import { FactSetAnchors, STUB_TITLE_CHARS, stubLabel } from './factsetAnchors';
import type { FactSetLink, FactSetNode } from './factsetApi';

function node(unit: number, anchor: string, written = true, extra: Partial<FactSetNode> = {}): FactSetNode {
  return {
    n: 1, path: `kb/z/${anchor}.md`, repo: 'r', commit: 'c', depth: 0, role: 'ref', title: anchor, type: 'observation',
    kind: 'epistemic', confidence: 0.7, anchor_id: anchor, unit, number: String(unit), tree_depth: 0, written, ...extra,
  };
}
const link = (anchor: string, direction: 'forward' | 'back', unit?: number): FactSetLink =>
  ({ anchor_id: anchor, direction, unit, cited_blob: 'b' });

// Test 3 (plan 7.3, simplified by the 2026-11-02 ruling): a link never shows a
// number; the resolver only decides the click. Fixture: unit 1 wrote f-a (two
// pages before the reader), unit 6 wrote f-b, a seek to unit 9 replaced the
// run on screen (so f-a and f-b are known but not on screen), unit 9 wrote
// f-c (on screen), and f-d is written in unit 12, not loaded yet.
//
// Sabotage: resolve a back link from the map before trusting link.unit (a
// target registered in an older unit), or scroll whenever the anchor is
// registered (ignoring whether it is on screen).
describe('FactSetAnchors.resolve (test 3)', () => {
  const a = new FactSetAnchors();
  a.register(node(1, 'f-a'));
  a.register(node(6, 'f-b'));
  a.register(node(9, 'f-c'));
  a.register(node(9, 'f-link', false));        // a stub is never a target
  a.register(node(10, 'f-gone', false, { unavailable: true })); // an unavailable root is
  const onScreen = (x: string) => x === 'f-c' || x === 'f-gone';

  it('a back link whose target is on screen scrolls to it', () => {
    expect(a.resolve(link('f-c', 'back', 9), onScreen)).toEqual({ kind: 'scroll', anchor: 'f-c' });
  });

  it('a back link whose target was read in a dropped run seeks to the unit the link names, then scrolls', () => {
    expect(a.resolve(link('f-b', 'back', 6), onScreen)).toEqual({ kind: 'seek', unit: 6, anchor: 'f-b' });
  });

  it('a back link whose target was never loaded (units before a seek) seeks to link.unit', () => {
    expect(a.unitOf('f-x')).toBeUndefined();
    expect(a.resolve(link('f-x', 'back', 3), onScreen)).toEqual({ kind: 'seek', unit: 3, anchor: 'f-x' });
  });

  it('the link unit wins over a stale map entry', () => {
    expect(a.resolve(link('f-a', 'back', 2), onScreen)).toEqual({ kind: 'seek', unit: 2, anchor: 'f-a' });
  });

  it('a forward link before its target arrives seeks to its unit; after, it scrolls', () => {
    expect(a.resolve(link('f-d', 'forward', 12), onScreen)).toEqual({ kind: 'seek', unit: 12, anchor: 'f-d' });
    const b = new FactSetAnchors();
    b.register(node(12, 'f-d'));
    expect(b.resolve(link('f-d', 'forward', 12), x => x === 'f-d')).toEqual({ kind: 'scroll', anchor: 'f-d' });
  });

  it('a stub is not registered as a target, an unavailable root is; no unit anywhere is no move', () => {
    expect(a.unitOf('f-link')).toBeUndefined();
    expect(a.unitOf('f-gone')).toBe(10);
    expect(a.resolve(link('f-none', 'back'), onScreen)).toEqual({ kind: 'none' });
    // An older server's back link without a unit still seeks by the map.
    expect(a.resolve(link('f-a', 'back'), onScreen)).toEqual({ kind: 'seek', unit: 1, anchor: 'f-a' });
  });
});

// The stub label: "see" plus a very short title (the length is a placeholder).
// Sabotage: return the number instead of the title, or never cut.
describe('stubLabel', () => {
  it('keeps a short title whole and cuts a long one at a word boundary', () => {
    expect(stubLabel('Section numbers')).toBe('see Section numbers');
    const long = 'A FactSet back link carries the unit that first wrote its target';
    const got = stubLabel(long);
    expect(got.startsWith('see A FactSet back link')).toBe(true);
    expect(got.endsWith('…')).toBe(true);
    expect(got.length).toBeLessThanOrEqual('see '.length + STUB_TITLE_CHARS + 1);
    expect(got).toBe('see A FactSet back link carries the…');
  });
});
