// The motif tier is a way of looking, so the history has to carry it.
//
// `kb/invariants/ui/navigation/nav-entry-captures-ways-of-looking` states the
// rule and records how it was gotten wrong last time: a move implemented in the
// component as two dispatches, neither of which pushed, so Back restored half
// the view and the reader could not get out. The tests here are that invariant
// applied to `motifMatch` — a field that changes WHICH ROWS EXIST, and which
// therefore needs a line in NavEntry, in pushNav, and in BOTH NAV_BACK arms.
// "Adding it to AppState alone silently makes Back lie."

import { describe, it, expect } from 'vitest';
import { reducer, init } from './state';
import type { AppState } from './state';

const at = (over: Partial<AppState> = {}): AppState => ({
  ...init, repo: 'knomit-kb', branch: 'agent/test',
  context: { kind: 'repo', repo: 'knomit-kb' }, ...over,
});

describe('PIVOT_MOTIF', () => {
  it('sets chip, sort, tier and open fact in ONE arm, with exactly one push', () => {
    const before = at({
      librarySort: 'path',
      factPath: 'kb/gotchas/store/testing/searchoptions-zero-limit/71123f5f.md',
      filters: [{ category: 'path', value: 'kb/gotchas' }],
    });
    const after = reducer(before, { type: 'PIVOT_MOTIF', motif: 'failure-presents-as-success' });

    expect(after.filters).toEqual([{ category: 'motif', value: 'failure-presents-as-success' }]);
    expect(after.librarySort).toBe('recent');
    expect(after.motifMatch).toBe('exact');
    expect(after.factPath).toBeNull();
    // Exactly one — a delta, not "non-empty". Two dispatches for one intent is
    // the smell the invariant names; so is a move that pushes twice.
    expect(after.navStack.length - before.navStack.length).toBe(1);
  });

  it('starts every pivot at the exact tier, even from a widened one', () => {
    const widened = at({ motifMatch: 'token-2', filters: [{ category: 'motif', value: 'a' }] });
    expect(reducer(widened, { type: 'PIVOT_MOTIF', motif: 'b' }).motifMatch).toBe('exact');
  });
});

describe('SET_MOTIF_MATCH', () => {
  it('pushes exactly once — widening is a move, not a preference', () => {
    const before = at({ filters: [{ category: 'motif', value: 'm' }] });
    const after = reducer(before, { type: 'SET_MOTIF_MATCH', match: 'token-2' });
    expect(after.motifMatch).toBe('token-2');
    expect(after.navStack.length - before.navStack.length).toBe(1);
  });

  it('is a no-op when the tier is already the one asked for', () => {
    // Otherwise re-clicking the lit rung buries the exact list under a stack of
    // identical entries and Back stops appearing to do anything.
    const s = at({ motifMatch: 'stem' });
    const after = reducer(s, { type: 'SET_MOTIF_MATCH', match: 'stem' });
    expect(after).toBe(s);
  });
});

describe('NAV_BACK restores the tier', () => {
  it('widened → exact, then → the fact pivoted from', () => {
    const start = at({
      factPath: 'kb/gotchas/testing/go/sort-stability-fixtures/13d5b9a9.md',
      librarySort: 'recent',
    });
    const pivoted = reducer(start, { type: 'PIVOT_MOTIF', motif: 'failure-presents-as-success' });
    const widened = reducer(pivoted, { type: 'SET_MOTIF_MATCH', match: 'token-2' });
    expect(widened.motifMatch).toBe('token-2');

    const back1 = reducer(widened, { type: 'NAV_BACK' });
    expect(back1.motifMatch).toBe('exact');
    expect(back1.filters).toEqual([{ category: 'motif', value: 'failure-presents-as-success' }]);

    const back2 = reducer(back1, { type: 'NAV_BACK' });
    expect(back2.factPath).toBe('kb/gotchas/testing/go/sort-stability-fixtures/13d5b9a9.md');
    expect(back2.filters).toEqual([]);
  });

  it('restores the tier on the repo-CHANGED arm too', () => {
    // Two arms, two chances to forget. The repo-changed branch resets most of
    // the view, and a tier left behind there would filter the new repo's list
    // by a setting the reader cannot see and did not choose.
    const before = at({ motifMatch: 'exact' });
    const pushed = reducer(before, { type: 'SET_MOTIF_MATCH', match: 'stem' });
    const elsewhere: AppState = { ...pushed, repo: 'other-kb', motifMatch: 'token-2' };
    const back = reducer(elsewhere, { type: 'NAV_BACK' });
    expect(back.repo).toBe('knomit-kb');
    expect(back.motifMatch).toBe('exact');
  });
});
