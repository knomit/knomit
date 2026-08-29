// A motif pivot is a WAY OF LOOKING, so the history has to carry it and the
// way back out has to land you where you were.
//
// `kb/invariants/ui/navigation/nav-entry-captures-ways-of-looking` states the
// rule and records how it was gotten wrong last time: a move implemented in the
// component as two dispatches, neither of which pushed, so Back restored half
// the view and the reader could not get out.
//
// The pivot is now the fourth MODE (path | recent | relevance | motif) and, like
// relevance, it is DERIVED rather than stored: `motifMatch` and its rungs are
// gone, and what makes the list a pivot is the motif chip alone. That moves the
// weight of these tests onto one property — the mode the reader came FROM must
// survive the round trip — which is exactly what a reducer that writes
// `librarySort` on the way in destroys.

import { describe, it, expect } from 'vitest';
import { reducer, init } from './state';
import type { AppState } from './state';

const at = (over: Partial<AppState> = {}): AppState => ({
  ...init, repo: 'knomit-kb', branch: 'agent/test',
  context: { kind: 'repo', repo: 'knomit-kb' }, ...over,
});

const MOTIF = 'failure-presents-as-success';

describe('PIVOT_MOTIF', () => {
  it('sets the chip, drops path and the open fact in ONE arm, with exactly one push', () => {
    const before = at({
      librarySort: 'path',
      factPath: 'kb/gotchas/store/testing/searchoptions-zero-limit/71123f5f.md',
      filters: [{ category: 'path', value: 'kb/gotchas' }],
    });
    const after = reducer(before, { type: 'PIVOT_MOTIF', motif: MOTIF });

    expect(after.filters).toEqual([{ category: 'motif', value: MOTIF }]);
    expect(after.factPath).toBeNull();
    // Exactly one — a delta, not "non-empty". Two dispatches for one intent is
    // the smell the invariant names; so is a move that pushes twice.
    expect(after.navStack.length - before.navStack.length).toBe(1);
  });

  it('does NOT write librarySort — the mode the reader came from is the record', () => {
    // The sort the pivot is READ in is derived (a motif chip is a content
    // filter, the tree cannot honour one, so Library overrides Path → Recent
    // for its duration). Storing that derived value here would overwrite the
    // only record of where the reader came from, and leaving the pivot could
    // then only ever drop them in Recent. This is the same failure EXIT_SEARCH
    // was written to avoid, one mode over.
    expect(reducer(at({ librarySort: 'path' }), { type: 'PIVOT_MOTIF', motif: MOTIF }).librarySort).toBe('path');
    expect(reducer(at({ librarySort: 'recent' }), { type: 'PIVOT_MOTIF', motif: MOTIF }).librarySort).toBe('recent');
  });

  it('REPLACES a previous motif chip rather than accumulating one', () => {
    // Two motif chips are an OR-union, which is the opposite of "show me this
    // shape" — and the header would have no single honest name to print.
    const after = reducer(
      at({ filters: [{ category: 'motif', value: 'a' }] }),
      { type: 'PIVOT_MOTIF', motif: 'b' },
    );
    expect(after.filters).toEqual([{ category: 'motif', value: 'b' }]);
  });
});

describe('EXIT_MOTIF', () => {
  it('drops the motif chip and the open fact, pushing exactly once', () => {
    const before = reducer(at({ librarySort: 'recent' }), { type: 'PIVOT_MOTIF', motif: MOTIF });
    const after = reducer({ ...before, factPath: 'kb/gotchas/x/1.md' }, { type: 'EXIT_MOTIF' });

    expect(after.filters).toEqual([]);
    expect(after.factPath).toBeNull();
    expect(after.navStack.length - before.navStack.length).toBe(1);
  });

  it('keeps chips the reader added to NARROW the pivot', () => {
    // EXIT_SEARCH drops every non-path chip because a search's refinements
    // belong to the search. A pivot's do not: a domain chip added inside a
    // pivot says where you are looking, not what you are looking at, so
    // leaving the shape must not also throw away the narrowing.
    const pivoted = reducer(at(), { type: 'PIVOT_MOTIF', motif: MOTIF });
    const narrowed = reducer(pivoted, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'store' } });
    const after = reducer(narrowed, { type: 'EXIT_MOTIF' });
    expect(after.filters).toEqual([{ category: 'domain', value: 'store' }]);
  });

  it('is a no-op with no motif chip set — nothing to leave, nothing to push', () => {
    // The segment only renders during a pivot, so this is defence against a
    // stray dispatch burying the view under an entry that changed nothing.
    const s = at({ filters: [{ category: 'domain', value: 'store' }] });
    expect(reducer(s, { type: 'EXIT_MOTIF' })).toBe(s);
  });
});

describe('the round trip', () => {
  it('an ontology browser who pivots and leaves is back in the ontology', () => {
    // THE REGRESSION THIS FILE EXISTS FOR. While PIVOT_MOTIF wrote
    // librarySort:'recent', this could not pass by construction: the reader
    // browsing the tree pivoted on a shape, left it, and landed in a flat
    // chronological feed they had never asked for — with nothing on screen
    // recording that they had come from the tree.
    const browsing = at({ librarySort: 'path', filters: [{ category: 'path', value: 'kb/gotchas' }] });
    const pivoted = reducer(browsing, { type: 'PIVOT_MOTIF', motif: MOTIF });
    const left = reducer(pivoted, { type: 'EXIT_MOTIF' });
    expect(left.librarySort).toBe('path');
    expect(left.filters).toEqual([]);
  });

  it('NAV_BACK out of a pivot restores the fact it was entered from', () => {
    const start = at({
      factPath: 'kb/gotchas/testing/go/sort-stability-fixtures/13d5b9a9.md',
      librarySort: 'recent',
    });
    const pivoted = reducer(start, { type: 'PIVOT_MOTIF', motif: MOTIF });
    const back = reducer(pivoted, { type: 'NAV_BACK' });
    expect(back.factPath).toBe('kb/gotchas/testing/go/sort-stability-fixtures/13d5b9a9.md');
    expect(back.filters).toEqual([]);
  });

  it('NAV_BACK undoes the exit too, returning to the pivot', () => {
    // Leaving is a move, so Back has to come back — otherwise the one gesture
    // out of a mode is also the one gesture that cannot be undone.
    const pivoted = reducer(at({ librarySort: 'path' }), { type: 'PIVOT_MOTIF', motif: MOTIF });
    const left = reducer(pivoted, { type: 'EXIT_MOTIF' });
    const back = reducer(left, { type: 'NAV_BACK' });
    expect(back.filters).toEqual([{ category: 'motif', value: MOTIF }]);
  });
});
