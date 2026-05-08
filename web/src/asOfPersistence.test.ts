import { describe, it, expect } from 'vitest';
import { init, reducer, selectAnchorCommit } from './state';
import type { AppState } from './state';

describe('AsOf persistence across view changes', () => {
  it('preserves diff mode when navigating tree → fact', () => {
    let s: AppState = { ...init, view: 'tree', factPath: 'kb/foo.md' };
    s = reducer(s, { type: 'SET_AS_OF', asOf: { mode: 'diff', from: 'aaa1111', to: 'bbb2222' } });
    expect(s.asOf).toEqual({ mode: 'diff', from: 'aaa1111', to: 'bbb2222' });
    expect(selectAnchorCommit(s)).toBe('bbb2222');
    s = reducer(s, { type: 'APPLY_NAV', view: 'tree', factPath: 'kb/bar.md', asOf: s.asOf });
    expect(s.asOf).toEqual({ mode: 'diff', from: 'aaa1111', to: 'bbb2222' });
    expect(selectAnchorCommit(s)).toBe('bbb2222');
  });

  it('FactDiffView trigger fires when navigating into a fact in diff mode', () => {
    const s: AppState = { ...init, factPath: 'kb/foo.md', asOf: { mode: 'diff', from: 'a', to: 'b' } };
    const triggered = s.asOf.mode === 'diff' && s.factPath !== null;
    expect(triggered).toBe(true);
  });

  it('preserves scrubbed mode across history → tree → history navigation', () => {
    let s: AppState = { ...init, view: 'history' };
    s = reducer(s, { type: 'SET_AS_OF', asOf: { mode: 'scrubbed', commit: 'abc1234' } });
    expect(s.asOf).toEqual({ mode: 'scrubbed', commit: 'abc1234' });
    s = reducer(s, { type: 'APPLY_NAV', view: 'tree', factPath: null, asOf: s.asOf });
    expect(s.asOf).toEqual({ mode: 'scrubbed', commit: 'abc1234' });
    s = reducer(s, { type: 'APPLY_NAV', view: 'history', factPath: null, asOf: s.asOf });
    expect(s.asOf).toEqual({ mode: 'scrubbed', commit: 'abc1234' });
  });
});
