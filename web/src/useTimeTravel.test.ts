import { describe, it, expect, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { resolveHopAnchor, computeReturnToNow, useTimeTravel } from './useTimeTravel';
import { planTrailHop, init } from './state';
import type { AppState } from './state';
import { api } from './api';
import type { Fact } from './api';
import type { TrailCrumb } from './state';

// The hook's async navigations classify their anchor via api.fact before
// dispatching; mock it so we control resolution timing in the race tests.
vi.mock('./api', () => ({ api: { fact: vi.fn() } }));

const crumb = (factPath: string): TrailCrumb => ({ factPath, asOf: { mode: 'live' } });

describe('planTrailHop', () => {
  it('pushes when the target is not already in the trail', () => {
    const trail = [crumb('kb/a.md'), crumb('kb/b.md')];
    expect(planTrailHop(trail, 'kb/c.md')).toEqual({ kind: 'push' });
  });
  it('unwinds to an earlier crumb instead of re-pushing (collapses A>B>A cycles)', () => {
    // Viewing B (depth 1) and hopping back to A (index 0) unwinds one step.
    const trail = [crumb('kb/a.md'), crumb('kb/b.md')];
    expect(planTrailHop(trail, 'kb/a.md')).toEqual({ kind: 'unwind', steps: 1 });
  });
  it('unwinds the full distance for a deeper revisit', () => {
    const trail = [crumb('kb/a.md'), crumb('kb/b.md'), crumb('kb/c.md')];
    expect(planTrailHop(trail, 'kb/a.md')).toEqual({ kind: 'unwind', steps: 2 });
  });
  it('is a no-op (unwind 0) when the target is already the current crumb', () => {
    const trail = [crumb('kb/a.md'), crumb('kb/b.md')];
    expect(planTrailHop(trail, 'kb/b.md')).toEqual({ kind: 'unwind', steps: 0 });
  });
});

const mkFact = (commit_hash: string): Fact => ({
  path: 'kb/b.md', title: 'B', body: '', domain: [], confidence: 0, sources: 0,
  entities: [], refs: [], commit_hash,
});

describe('resolveHopAnchor', () => {
  const live = { mode: 'live' } as const;
  it('from live: a live target stays live, even when the edge pinned an older version', async () => {
    // The HEAD read succeeds (target exists at HEAD), so following the ref from
    // live shows the live target. A "superseded" target (changed since the edge
    // was formed) is still live and must NOT drop the UI into history.
    const fact = vi.fn(async () => mkFact('head777'));
    const r = await resolveHopAnchor('r', 'b', 'kb/b.md', 'pin111', live, { fact: fact as any });
    expect(r.asOf).toEqual({ mode: 'live' });
    // Only the HEAD read is needed to tell live from retracted.
    expect(fact).toHaveBeenCalledTimes(1);
    expect(fact).toHaveBeenCalledWith('r', 'b', 'kb/b.md');
  });
  it('from live: a retracted target (HEAD 404) -> history at the pinned commit', async () => {
    const fact = vi.fn(async () => { throw new Error('404'); }); // HEAD read 404s
    const r = await resolveHopAnchor('r', 'b', 'kb/b.md', 'pin111', live, { fact: fact as any });
    expect(r.asOf).toEqual({ mode: 'history', commit: 'pin111' });
    expect(fact).toHaveBeenCalledTimes(1);
  });
  it('from a history excursion: stays history at the pinned commit, no HEAD read', async () => {
    // Already time-travelling — keep the excursion anchored at the edge's commit.
    // No HEAD read is required (or wanted) to make that choice.
    const fact = vi.fn(async () => mkFact('head777'));
    const r = await resolveHopAnchor(
      'r', 'b', 'kb/b.md', 'pin111', { mode: 'history', commit: 'cX' }, { fact: fact as any });
    expect(r.asOf).toEqual({ mode: 'history', commit: 'pin111' });
    expect(fact).not.toHaveBeenCalled();
  });
});

describe('computeReturnToNow', () => {
  it('subject present at HEAD -> stays subject, live', async () => {
    const fact = vi.fn(async () => mkFact('head1'));
    const r = await computeReturnToNow('r', 'b', 'kb/x/y.md', { fact: fact as any });
    expect(r).toEqual({ kind: 'subject', factPath: 'kb/x/y.md' });
  });
  it('subject retracted -> parent folder + notice', async () => {
    const fact = vi.fn(async () => { throw new Error('404'); });
    const r = await computeReturnToNow('r', 'b', 'kb/x/y.md', { fact: fact as any });
    expect(r).toEqual({
      kind: 'parent',
      parentPath: 'kb/x',
      notice: '"kb/x/y.md" was retracted — no live version. Returned to now.',
    });
  });
});

// Regression: an in-flight async hop must not clobber a newer navigation that
// started while it was resolving. resolveHopAnchor reads api.fact over the
// network before dispatching, so without a generation guard the later-resolving
// (not last-clicked) navigation would win.
describe('useTimeTravel stale-navigation guard', () => {
  const baseState = (): AppState => ({ ...init, repo: 'r', branch: 'b', headCommit: 'head1' });
  const navsFrom = (dispatch: ReturnType<typeof vi.fn>) =>
    dispatch.mock.calls.map(c => c[0]).filter((a: any) => a.type === 'APPLY_NAV');

  it('a later hop wins even when an earlier hop resolves last', async () => {
    let resolveA!: (f: Partial<Fact>) => void;
    let resolveB!: (f: Partial<Fact>) => void;
    (api.fact as any)
      .mockImplementationOnce(() => new Promise(res => { resolveA = res; }))
      .mockImplementationOnce(() => new Promise(res => { resolveB = res; }));

    const dispatch = vi.fn();
    const { result } = renderHook(() => useTimeTravel(baseState(), dispatch as any));

    let callA!: Promise<void>;
    let callB!: Promise<void>;
    act(() => {
      callA = result.current.hopEdge('kb/a.md', 'pinA'); // generation 1
      callB = result.current.hopEdge('kb/b.md', 'pinB'); // generation 2 (newest)
    });

    // Resolve the newest first, then the stale one — the stale resolve must be
    // dropped rather than overwrite B.
    await act(async () => { resolveB({ commit_hash: 'headB' }); await callB; });
    await act(async () => { resolveA({ commit_hash: 'headA' }); await callA; });

    const navs = navsFrom(dispatch);
    expect(navs).toHaveLength(1);
    expect(navs[0].factPath).toBe('kb/b.md');
  });

  it('a synchronous scrub supersedes an in-flight hop', async () => {
    let resolveHop!: (f: Partial<Fact>) => void;
    (api.fact as any).mockImplementationOnce(() => new Promise(res => { resolveHop = res; }));

    const dispatch = vi.fn();
    const { result } = renderHook(() => useTimeTravel(baseState(), dispatch as any));

    let call!: Promise<void>;
    act(() => { call = result.current.hopEdge('kb/a.md', 'pinA'); }); // generation 1
    act(() => { result.current.scrub('commitX'); });                 // generation 2
    await act(async () => { resolveHop({ commit_hash: 'headA' }); await call; });

    expect(navsFrom(dispatch)).toHaveLength(0); // hop dropped as stale
    const setAsOf = dispatch.mock.calls.map(c => c[0]).filter((a: any) => a.type === 'SET_AS_OF');
    expect(setAsOf).toEqual([{ type: 'SET_AS_OF', asOf: { mode: 'history', commit: 'commitX' } }]);
  });
});
