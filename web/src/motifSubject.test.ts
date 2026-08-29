// "The subject" of a fact, for the pivot's spread line.
//
// It is path segment 3 — `kb/<topic>/<SUBJECT>/…` — and explicitly NOT the
// `domain` field. Measured on this repository's own knowledge base: `domain[0]`
// disagrees with the path segment on 209 of 1,183 facts, 82 facts carry no
// domain at all, and taking every domain gives 53 labels across the 26 carriers
// of one motif — more variety than the list has facts. The path segment is
// single-valued, always present, and is what the design's figures were measured
// from.
//
// The summary line follows the fact header's grammar: whole names or none. A
// motif name clipped mid-phrase inverts its claim, and while a subject is only
// a word, using one rule in one place and another next to it is how two
// surfaces drift.

import { describe, it, expect } from 'vitest';
import { factSubject, subjectSummary } from './motifSubject';

describe('factSubject', () => {
  it('is the third path segment', () => {
    expect(factSubject('kb/gotchas/store/testing/searchoptions-zero-limit/71123f5f.md')).toBe('store');
    expect(factSubject('kb/invariants/build/skip-if-present/71ab5425.md')).toBe('build');
  });

  it('reads the path even when the fact carries a different first domain', () => {
    // The divergence case, with a real shape: this fact's domains lead with
    // "repos" while it lives under `testing`. Two different answers; the path
    // is the one the design measured.
    expect(factSubject('kb/gotchas/testing/storyboard/local-origin-gate/527fbafa.md')).toBe('testing');
  });

  it('handles a lens-qualified path, which carries a mount prefix', () => {
    expect(factSubject('kb://3ec012f5b4d2/kb/decisions/ui/filter-bar/x.md')).toBe('ui');
  });

  it('returns empty for a path too short to have a subject', () => {
    // A guard that renders nothing beats one that renders "undefined".
    expect(factSubject('kb/gotchas/x.md')).toBe('');
    expect(factSubject('kb')).toBe('');
    expect(factSubject('')).toBe('');
  });
});

describe('subjectSummary', () => {
  const paths = (...subjects: string[]) => subjects.map((s, i) => `kb/gotchas/${s}/f${i}/x.md`);

  it('orders by how many facts carry each subject, then by name', () => {
    // The FacetPanel's tie-break, reused: without the name tie-break two
    // renders of the same list are free to disagree about which of two equal
    // subjects leads.
    const r = subjectSummary(paths('build', 'store', 'store', 'build', 'apple'), 3);
    expect(r.shown).toEqual(['build', 'store', 'apple']);
    expect(r.more).toBe(0);
  });

  it('counts what it dropped rather than showing part of a name', () => {
    const r = subjectSummary(paths('a', 'b', 'c', 'd', 'e'), 3);
    expect(r.shown).toHaveLength(3);
    expect(r.more).toBe(2);
  });

  it('never emits a partial name', () => {
    const r = subjectSummary(paths('methodology', 'claude-code', 'synthesize'), 2);
    for (const s of r.shown) expect(s).not.toContain('…');
    expect(r.shown.every(s => s.length > 0)).toBe(true);
  });

  it('ignores facts with no readable subject rather than counting a blank one', () => {
    const r = subjectSummary(['kb/gotchas/store/a/x.md', 'kb/short.md', ''], 5);
    expect(r.shown).toEqual(['store']);
    expect(r.more).toBe(0);
  });

  it('is empty for an empty list', () => {
    expect(subjectSummary([], 3)).toEqual({ shown: [], more: 0, total: 0 });
  });

  it('reports the total number of distinct subjects', () => {
    // The caller needs this to say "+N more" without recounting, and the count
    // must be of distinct subjects, not of facts.
    const r = subjectSummary(paths('a', 'a', 'a', 'b'), 1);
    expect(r.total).toBe(2);
    expect(r.more).toBe(1);
  });
});
