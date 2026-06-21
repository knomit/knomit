import { describe, it, expect } from 'vitest';
import { init, reducer, selectAnchorCommit, isLive, isReadOnly } from './state';

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

  it('does not push navStack', () => {
    const next = reducer(init, { type: 'SET_AS_OF', asOf: { mode: 'history', commit: 'abc1234' } });
    expect(next.navStack.length).toBe(init.navStack.length);
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
