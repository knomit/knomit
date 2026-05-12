import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { init, reducer, selectAnchorCommit, isLive, isReadOnly } from './state';

describe('AsOf selectors', () => {
  it('selectAnchorCommit returns null in live mode', () => {
    expect(selectAnchorCommit(init)).toBeNull();
  });

  it('selectAnchorCommit returns commit in scrubbed mode', () => {
    const s = { ...init, asOf: { mode: 'scrubbed' as const, commit: 'abc1234' } };
    expect(selectAnchorCommit(s)).toBe('abc1234');
  });

  it('selectAnchorCommit returns to in diff mode', () => {
    const s = { ...init, asOf: { mode: 'diff' as const, from: 'aaa1111', to: 'bbb2222' } };
    expect(selectAnchorCommit(s)).toBe('bbb2222');
  });

  it('isLive returns true in live mode', () => {
    expect(isLive(init)).toBe(true);
  });

  it('isLive returns false in scrubbed mode', () => {
    const s = { ...init, asOf: { mode: 'scrubbed' as const, commit: 'abc1234' } };
    expect(isLive(s)).toBe(false);
  });

  it('isLive returns false in diff mode', () => {
    const s = { ...init, asOf: { mode: 'diff' as const, from: 'a', to: 'b' } };
    expect(isLive(s)).toBe(false);
  });

  it('isReadOnly is the negation of isLive', () => {
    expect(isReadOnly(init)).toBe(false);
    const s1 = { ...init, asOf: { mode: 'scrubbed' as const, commit: 'abc1234' } };
    expect(isReadOnly(s1)).toBe(true);
    const s2 = { ...init, asOf: { mode: 'diff' as const, from: 'a', to: 'b' } };
    expect(isReadOnly(s2)).toBe(true);
  });
});

describe('reducer — SET_AS_OF', () => {
  it('sets asOf to live', () => {
    const s = { ...init, asOf: { mode: 'scrubbed' as const, commit: 'abc1234' } };
    const next = reducer(s, { type: 'SET_AS_OF', asOf: { mode: 'live' } });
    expect(next.asOf).toEqual({ mode: 'live' });
  });

  it('sets asOf to scrubbed', () => {
    const next = reducer(init, { type: 'SET_AS_OF', asOf: { mode: 'scrubbed', commit: 'abc1234' } });
    expect(next.asOf).toEqual({ mode: 'scrubbed', commit: 'abc1234' });
  });

  it('sets asOf to diff', () => {
    const next = reducer(init, { type: 'SET_AS_OF', asOf: { mode: 'diff', from: 'a', to: 'b' } });
    expect(next.asOf).toEqual({ mode: 'diff', from: 'a', to: 'b' });
  });

  it('does not push navStack', () => {
    const next = reducer(init, { type: 'SET_AS_OF', asOf: { mode: 'scrubbed', commit: 'abc1234' } });
    expect(next.navStack.length).toBe(init.navStack.length);
  });
});

describe('reducer — boundary preservation', () => {
  it('APPLY_NAV with view unchanged does not lose dispatched asOf', () => {
    const s = { ...init, view: 'history' as const };
    // Dispatched asOf must survive — boundary-clear branch must not fire on history → history.
    const next = reducer(s, {
      type: 'APPLY_NAV',
      view: 'history',
      factPath: null,
      asOf: { mode: 'scrubbed', commit: 'abc1234' },
    });
    expect(next.asOf).toEqual({ mode: 'scrubbed', commit: 'abc1234' });
  });

  it('APPLY_NAV preserves asOf when crossing tree → history', () => {
    const s = {
      ...init,
      view: 'tree' as const,
      asOf: { mode: 'scrubbed' as const, commit: 'aaa1111' },
    };
    const next = reducer(s, {
      type: 'APPLY_NAV',
      view: 'history',
      factPath: null,
      asOf: { mode: 'scrubbed', commit: 'bbb2222' },
    });
    expect(next.asOf).toEqual({ mode: 'scrubbed', commit: 'bbb2222' });
  });

  it('APPLY_NAV preserves diff mode across view changes', () => {
    const s = {
      ...init,
      view: 'tree' as const,
      factPath: 'kb/foo.md',
      asOf: { mode: 'diff' as const, from: 'aaa1111', to: 'bbb2222' },
    };
    const next = reducer(s, {
      type: 'APPLY_NAV',
      view: 'history',
      factPath: 'kb/foo.md',
      asOf: s.asOf,
    });
    expect(next.asOf).toEqual({ mode: 'diff', from: 'aaa1111', to: 'bbb2222' });
  });
});

describe('reducer — flag-off enforcement', () => {
  beforeEach(() => {
    vi.stubEnv('VITE_TEMPORAL_ENABLED', 'false');
    vi.resetModules();
  });
  afterEach(() => {
    vi.unstubAllEnvs();
    vi.resetModules();
  });

  it('SET_AS_OF refuses non-live payload when flag is off', async () => {
    const mod = await import('./state');
    const next = mod.reducer(mod.init, {
      type: 'SET_AS_OF',
      asOf: { mode: 'scrubbed', commit: 'abc1234' },
    });
    expect(next.asOf).toEqual({ mode: 'live' });
  });

  it('SET_AS_OF still allows live payload when flag is off', async () => {
    const mod = await import('./state');
    const s = { ...mod.init, asOf: { mode: 'scrubbed' as const, commit: 'abc1234' } };
    // The state shouldn't be reachable with the flag off, but the reducer
    // must still accept a SET_AS_OF→live to repair it.
    const next = mod.reducer(s, { type: 'SET_AS_OF', asOf: { mode: 'live' } });
    expect(next.asOf).toEqual({ mode: 'live' });
  });

  it('APPLY_NAV scrubs non-live asOf to live when flag is off', async () => {
    const mod = await import('./state');
    const next = mod.reducer(mod.init, {
      type: 'APPLY_NAV',
      view: 'history',
      factPath: 'kb/foo.md',
      asOf: { mode: 'diff', from: 'aaa1111', to: 'bbb2222' },
    });
    expect(next.asOf).toEqual({ mode: 'live' });
    // View/path changes still apply.
    expect(next.view).toBe('history');
    expect(next.factPath).toBe('kb/foo.md');
  });

  it('AMEND_NAV with scrubbed asOf is stripped to current asOf when flag is off', async () => {
    vi.stubEnv('VITE_TEMPORAL_ENABLED', 'false');
    vi.resetModules();
    const m = await import('./state');

    const start = { ...m.init, asOf: { mode: 'live' } as const };
    // factPath update is allowed; the scrubbed asOf payload is stripped.
    const next = m.reducer(start, {
      type: 'AMEND_NAV',
      factPath: 'kb/foo.md',
      asOf: { mode: 'scrubbed', commit: 'abc1234' },
    });
    expect(next.factPath).toBe('kb/foo.md');
    expect(next.asOf).toEqual({ mode: 'live' });
  });
});
