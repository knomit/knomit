import { describe, it, expect } from 'vitest';
import { init, reducer, selectAnchorCommit, selectTrail, isLive, isReadOnly } from './state';

describe('AsOf selectors', () => {
  it('selectAnchorCommit returns null in live mode', () => {
    expect(selectAnchorCommit(init)).toBeNull();
  });

  it('selectAnchorCommit returns commit in history mode', () => {
    const s = { ...init, asOf: { mode: 'history' as const, commit: 'abc1234' } };
    expect(selectAnchorCommit(s)).toBe('abc1234');
  });

  it('selectAnchorCommit returns to in diff mode', () => {
    const s = { ...init, asOf: { mode: 'diff' as const, from: 'aaa1111', to: 'bbb2222' } };
    expect(selectAnchorCommit(s)).toBe('bbb2222');
  });

  it('isLive returns true in live mode', () => {
    expect(isLive(init)).toBe(true);
  });

  it('isLive returns false in history mode', () => {
    const s = { ...init, asOf: { mode: 'history' as const, commit: 'abc1234' } };
    expect(isLive(s)).toBe(false);
  });

  it('isLive returns false in diff mode', () => {
    const s = { ...init, asOf: { mode: 'diff' as const, from: 'a', to: 'b' } };
    expect(isLive(s)).toBe(false);
  });

  it('isReadOnly is the negation of isLive', () => {
    expect(isReadOnly(init)).toBe(false);
    const s1 = { ...init, asOf: { mode: 'history' as const, commit: 'abc1234' } };
    expect(isReadOnly(s1)).toBe(true);
    const s2 = { ...init, asOf: { mode: 'diff' as const, from: 'a', to: 'b' } };
    expect(isReadOnly(s2)).toBe(true);
  });
});

describe('reducer — SET_AS_OF', () => {
  it('sets asOf to live', () => {
    const s = { ...init, asOf: { mode: 'history' as const, commit: 'abc1234' } };
    const next = reducer(s, { type: 'SET_AS_OF', asOf: { mode: 'live' } });
    expect(next.asOf).toEqual({ mode: 'live' });
  });

  it('sets asOf to history', () => {
    const next = reducer(init, { type: 'SET_AS_OF', asOf: { mode: 'history', commit: 'abc1234' } });
    expect(next.asOf).toEqual({ mode: 'history', commit: 'abc1234' });
  });

  it('sets asOf to diff', () => {
    const next = reducer(init, { type: 'SET_AS_OF', asOf: { mode: 'diff', from: 'a', to: 'b' } });
    expect(next.asOf).toEqual({ mode: 'diff', from: 'a', to: 'b' });
  });

  it('does not push navStack when no fact is open', () => {
    const next = reducer(init, { type: 'SET_AS_OF', asOf: { mode: 'history', commit: 'abc1234' } });
    expect(next.navStack.length).toBe(init.navStack.length);
  });
});

// knomit#486: opening a fact's timeline is a drill-down, so Back (Backspace,
// the header arrow and the timeline arrow all dispatch NAV_BACK) must return to
// the fact. Only the live -> non-live ENTRY pushes; scrubbing and leaving a
// diff stay in place so they do not grow the trail.
describe('reducer — timeline entry is a navigation step (#486)', () => {
  const open = (factPath: string) =>
    reducer(init, { type: 'APPLY_NAV', view: 'library', factPath, asOf: { mode: 'live' } });
  const enter = (s: typeof init, commit = 'c3') =>
    reducer(s, { type: 'SET_AS_OF', asOf: { mode: 'history', commit } });

  it('NAV_BACK from the timeline returns to the open fact, live, timeline closed', () => {
    const opened = open('kb/a.md');
    const inTimeline = enter(opened);
    expect(inTimeline.asOf).toEqual({ mode: 'history', commit: 'c3' });
    const back = reducer(inTimeline, { type: 'NAV_BACK' });
    expect(back.factPath).toBe('kb/a.md');
    expect(back.asOf).toEqual({ mode: 'live' });
    expect(back.navStack).toEqual(opened.navStack);
    // A second Back is the pre-existing step: the fact is closed.
    expect(reducer(back, { type: 'NAV_BACK' }).factPath).toBeNull();
  });

  it('scrubbing between versions and leaving a diff push nothing', () => {
    const inTimeline = enter(open('kb/a.md'));
    const depth = inTimeline.navStack.length;
    const scrubbed = enter(inTimeline, 'c2');
    expect(scrubbed.navStack.length).toBe(depth);
    const diff = reducer(scrubbed, { type: 'SET_AS_OF', asOf: { mode: 'diff', from: 'c1', to: 'c2' } });
    expect(diff.navStack.length).toBe(depth);
    expect(reducer(diff, { type: 'SET_AS_OF', asOf: { mode: 'history', commit: 'c2' } }).navStack.length).toBe(depth);
  });

  it('keeps the trail one crumb per fact and each crumb jumpable by its pop count', () => {
    let s = enter(open('kb/a.md'));
    expect(selectTrail(s).map(c => c.factPath)).toEqual(['kb/a.md']);
    s = reducer(s, { type: 'APPLY_NAV', view: 'library', factPath: 'kb/b.md', asOf: { mode: 'history', commit: 'c9' }, hop: true });
    const trail = selectTrail(s);
    expect(trail.map(c => c.factPath)).toEqual(['kb/a.md', 'kb/b.md']);
    // Jumping to crumb 0 = the recorded pops, and lands on A live.
    let j = s;
    for (let k = 0; k < trail[0].pops!; k++) j = reducer(j, { type: 'NAV_BACK' });
    expect(j.factPath).toBe('kb/a.md');
    expect(j.asOf).toEqual({ mode: 'live' });
  });
});

describe('reducer — boundary preservation', () => {
  it('APPLY_NAV with same view does not lose dispatched asOf', () => {
    const s = { ...init, view: 'library' as const };
    // Dispatched asOf must survive unchanged when view stays the same.
    const next = reducer(s, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: null,
      asOf: { mode: 'history', commit: 'abc1234' },
    });
    expect(next.asOf).toEqual({ mode: 'history', commit: 'abc1234' });
  });

  it('APPLY_NAV preserves history asOf when factPath changes', () => {
    const s = {
      ...init,
      view: 'library' as const,
      asOf: { mode: 'history' as const, commit: 'aaa1111' },
    };
    const next = reducer(s, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: 'kb/foo.md',
      asOf: { mode: 'history', commit: 'bbb2222' },
    });
    expect(next.asOf).toEqual({ mode: 'history', commit: 'bbb2222' });
  });

  it('APPLY_NAV preserves diff mode across factPath changes', () => {
    const s = {
      ...init,
      view: 'library' as const,
      factPath: 'kb/foo.md',
      asOf: { mode: 'diff' as const, from: 'aaa1111', to: 'bbb2222' },
    };
    const next = reducer(s, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: 'kb/bar.md',
      asOf: s.asOf,
    });
    expect(next.asOf).toEqual({ mode: 'diff', from: 'aaa1111', to: 'bbb2222' });
  });
});

describe('temporal anchor (no flag)', () => {
  it('SET_AS_OF history is always honored', () => {
    const s = reducer(init, { type: 'SET_AS_OF', asOf: { mode: 'history', commit: 'abc1234' } });
    expect(s.asOf).toEqual({ mode: 'history', commit: 'abc1234' });
    expect(isLive(s)).toBe(false);
    expect(selectAnchorCommit(s)).toBe('abc1234');
  });
  it('live anchor resolves to null', () => {
    expect(selectAnchorCommit(init)).toBeNull();
    expect(isLive(init)).toBe(true);
  });
});
