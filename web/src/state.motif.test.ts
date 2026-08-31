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
import type { AppState, FilterChip } from './state';

const at = (over: Partial<AppState> = {}): AppState => ({
  ...init, repo: 'knomit-kb', branch: 'agent/test',
  context: { kind: 'repo', repo: 'knomit-kb' }, ...over,
});

const MOTIF = 'failure-presents-as-success';

describe('PIVOT_MOTIF', () => {
  it('sets the chip and drops path in ONE arm, with exactly one push', () => {
    const before = at({
      librarySort: 'path',
      factPath: 'kb/gotchas/store/testing/searchoptions-zero-limit/71123f5f.md',
      filters: [{ category: 'path', value: 'kb/gotchas' }],
    });
    const after = reducer(before, { type: 'PIVOT_MOTIF', motif: MOTIF });

    // The displaced path rides ON the chip. The pivot is right to drop it — a
    // shape cuts across the ontology — but the drop is bookkeeping the reader
    // never saw, and the ≈ segment promises "go back": the chip that IS the
    // pivot carries where it came from, so leaving can honour the promise.
    expect(after.filters).toEqual([{ category: 'motif', value: MOTIF, returnPath: 'kb/gotchas' }]);
    // Exactly one — a delta, not "non-empty". Two dispatches for one intent is
    // the smell the invariant names; so is a move that pushes twice.
    expect(after.navStack.length - before.navStack.length).toBe(1);
  });

  it('leaves the open fact ALONE — the list that arrives decides whether it survives', () => {
    // This arm used to clear factPath, on the grounds that the refetched list
    // would select its own first row. Every list branch already does that, and
    // only when the open fact did NOT survive the refetch (api.recent,
    // api.search and the lens rows each hold the same guard), so the clear
    // bought nothing and cost two bugs:
    //
    //   • Between the dispatch and the response the right panel had no fact
    //     to draw, so the ontology dashboard FLASHED up in the middle of a
    //     move that never meant to leave the fact.
    //   • When the pivot changed no query at all — the same motif pinned
    //     again from a carrier's own header — nothing refetched, so nothing
    //     re-selected, and the dashboard stayed: a dead end reached by a
    //     button that promised a list.
    //
    // Keeping it is also the better answer on the common path: you pivot on a
    // motif OF the fact you are reading, so that fact is a carrier, is in the
    // list, and reading it should not be interrupted to show you row 0.
    const open = 'kb/gotchas/store/testing/searchoptions-zero-limit/71123f5f.md';
    const after = reducer(at({ factPath: open }), { type: 'PIVOT_MOTIF', motif: MOTIF });
    expect(after.factPath).toBe(open);
  });

  it('is a no-op when the motif asked for is the one already pinned', () => {
    // Reached from a carrier's own header: the chip, the list and the fact are
    // already what the button offers, so there is nothing to do and nothing to
    // remember — pushing here would spend a Back press on a view that never
    // changed. Same defence as EXIT_MOTIF's stray-dispatch arm below.
    const pinned = at({
      factPath: 'kb/gotchas/store/testing/searchoptions-zero-limit/71123f5f.md',
      filters: [{ category: 'motif', value: MOTIF, returnPath: 'kb/gotchas' }],
    });
    expect(reducer(pinned, { type: 'PIVOT_MOTIF', motif: MOTIF })).toBe(pinned);
  });

  it('collapses a typed union even when the pivot names its first chip', () => {
    // Motif chips accumulate when TYPED into the FilterBar (ADD_FILTER appends
    // them), and two of them are a union — the one thing this arm exists to
    // undo. Reading "is this already pinned?" off the first matching chip
    // no-opped that collapse, and only when the reader had typed the pivoted
    // shape FIRST: same gesture, opposite outcome, decided by typing order.
    const union: FilterChip[] = [{ category: 'motif', value: 'a' }, { category: 'motif', value: 'b' }];
    for (const filters of [union, [...union].reverse()]) {
      const s = at({ filters });
      const after = reducer(s, { type: 'PIVOT_MOTIF', motif: 'a' });
      expect(after.filters).toEqual([{ category: 'motif', value: 'a' }]);
      expect(after.navStack.length - s.navStack.length).toBe(1);
    }
  });

  it('still pivots when the same motif is pinned but a path chip is there to drop', () => {
    // Not a no-op: the chip set is the same shape, but the path narrows the
    // list, so dropping it is a real change to what the reader is looking at
    // — and the displaced folder has to be stashed for the exit.
    const s = at({
      filters: [
        { category: 'motif', value: MOTIF },
        { category: 'path', value: 'kb/gotchas' },
      ],
    });
    const after = reducer(s, { type: 'PIVOT_MOTIF', motif: MOTIF });
    expect(after.filters).toEqual([{ category: 'motif', value: MOTIF, returnPath: 'kb/gotchas' }]);
    expect(after.navStack.length - s.navStack.length).toBe(1);
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

  it('carries the displaced path through a re-pivot', () => {
    // Pivot from kb/gotchas, then pivot again from inside the first pivot: the
    // second chip replaces the first, but the place the reader was LAST
    // STANDING is still kb/gotchas — no path chip existed to displace during
    // the re-pivot, so the first chip's stash is the record and must survive.
    const first = reducer(
      at({ filters: [{ category: 'path', value: 'kb/gotchas' }] }),
      { type: 'PIVOT_MOTIF', motif: 'a' },
    );
    const second = reducer(first, { type: 'PIVOT_MOTIF', motif: 'b' });
    expect(second.filters).toEqual([{ category: 'motif', value: 'b', returnPath: 'kb/gotchas' }]);
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

  it('restores the folder the pivot displaced', () => {
    // "Leave this motif and go back" — the segment's own words. PIVOT_MOTIF
    // dropped the path chip, so without the chip's stash this exit landed an
    // ontology browser at the ROOT: the mode came back, the place did not,
    // while the chevron one gesture away restored both. Two adjacent "back"s
    // must not land in different places.
    const pivoted = reducer(
      at({ filters: [{ category: 'path', value: 'kb/gotchas' }] }),
      { type: 'PIVOT_MOTIF', motif: MOTIF },
    );
    const left = reducer(pivoted, { type: 'EXIT_MOTIF' });
    expect(left.filters).toEqual([{ category: 'path', value: 'kb/gotchas' }]);
  });

  it('conjures no folder for a pivot entered from the root', () => {
    // Nothing was displaced, so nothing comes back: root in, root out.
    const pivoted = reducer(at(), { type: 'PIVOT_MOTIF', motif: MOTIF });
    expect(reducer(pivoted, { type: 'EXIT_MOTIF' }).filters).toEqual([]);
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
    // ...and in the FOLDER they were browsing, not at the root. The mode
    // half of this round trip shipped first; the place half is the chip's
    // returnPath doing its one job.
    expect(left.filters).toEqual([{ category: 'path', value: 'kb/gotchas' }]);
  });

  it('the stash survives history: back into a pivot, out again, same folder', () => {
    // pushNav snapshots filters, and the stash lives on the chip — so a pivot
    // re-entered through NAV_BACK still knows where it came from, with no
    // top-level field to go stale beside the stack.
    const browsing = at({ librarySort: 'path', filters: [{ category: 'path', value: 'kb/gotchas' }] });
    const pivoted = reducer(browsing, { type: 'PIVOT_MOTIF', motif: MOTIF });
    const left = reducer(pivoted, { type: 'EXIT_MOTIF' });
    const backInPivot = reducer(left, { type: 'NAV_BACK' });
    const leftAgain = reducer(backInPivot, { type: 'EXIT_MOTIF' });
    expect(leftAgain.filters).toEqual([{ category: 'path', value: 'kb/gotchas' }]);
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
