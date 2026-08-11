import { describe, it, expect, vi } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { computeReturnToNow, useTimeTravel } from './useTimeTravel';
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

// resolveHopAnchor is GONE. It read the target at the HEAD endpoint while live
// and, on 200, returned {mode:'live'} — discarding the edge's commit and opening
// whatever the target is NOW. A synthesis citing A@5902160 opened A@8f23800 the
// moment A was revised.
//
// There is nothing left to resolve: a reference resolves at the commit it was
// added at, always (kb/principles/philosophy/historical-not-current). hopEdge
// pins to the edge's commit and is synchronous. Its behaviour is asserted in
// the useTimeTravel blocks below; what USED to be tested here — "a live target
// stays live even when the edge pinned an older version" — was the bug, stated
// as a requirement.

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

// Regression: an in-flight async navigation must not clobber a newer one that
// started while it was resolving. hopEdge is SYNCHRONOUS now — it pins to the
// edge's commit and dispatches, with no network read to race — so the guard
// exists for returnToNow, which still resolves an anchor over the wire.
describe('useTimeTravel stale-navigation guard', () => {
  const baseState = (): AppState => ({ ...init, repo: 'r', branch: 'b', headCommit: 'head1' });
  const navsFrom = (dispatch: ReturnType<typeof vi.fn>) =>
    dispatch.mock.calls.map(c => c[0]).filter((a: any) => a.type === 'APPLY_NAV');

  it('two hops dispatch in click order, each pinned to its own edge commit', () => {
    const dispatch = vi.fn();
    const { result } = renderHook(() => useTimeTravel(baseState(), dispatch as any));
    act(() => {
      result.current.hopEdge('kb/a.md', 'pinA');
      result.current.hopEdge('kb/b.md', 'pinB');
    });
    const navs = navsFrom(dispatch);
    expect(navs.map((n: any) => [n.factPath, n.asOf])).toEqual([
      ['kb/a.md', { mode: 'history', commit: 'pinA' }],
      ['kb/b.md', { mode: 'history', commit: 'pinB' }],
    ]);
    // No HEAD read: the edge already says which commit to open.
    expect(api.fact).not.toHaveBeenCalled();
  });

  it('a hop after a scrub wins, and does not inherit the scrubbed commit', () => {
    const dispatch = vi.fn();
    const { result } = renderHook(() => useTimeTravel(baseState(), dispatch as any));
    act(() => { result.current.scrub('commitX'); });
    act(() => { result.current.hopEdge('kb/a.md', 'pinA'); });
    expect(navsFrom(dispatch)[0].asOf).toEqual({ mode: 'history', commit: 'pinA' });
  });

  it('an in-flight returnToNow is dropped when a hop supersedes it', async () => {
    let resolveReturn!: (f: Partial<Fact>) => void;
    (api.fact as any).mockImplementationOnce(() => new Promise(res => { resolveReturn = res; }));
    const dispatch = vi.fn();
    const { result } = renderHook(() => useTimeTravel(
      { ...baseState(), factPath: 'kb/x.md', asOf: { mode: 'history', commit: 'c1' } }, dispatch as any));

    let call!: Promise<void>;
    act(() => { call = result.current.returnToNow(); });   // generation 1, in flight
    act(() => { result.current.hopEdge('kb/a.md', 'pinA'); }); // generation 2
    await act(async () => { resolveReturn({ commit_hash: 'head1' }); await call; });

    // Only the hop committed; the stale returnToNow was dropped.
    expect(navsFrom(dispatch).map((n: any) => n.factPath)).toEqual(['kb/a.md']);
  });
});

// Task 17: in a lens context the temporal fetches must anchor on the OPEN
// FACT's source mount (openFactSource) and read via the mount's repo-scoped
// endpoints with the RELATIVE path (kb://<id12>/ stripped). In a repo context
// this collapses to {state.repo, state.branch} + the bare path (unchanged).
describe('useTimeTravel — per-fact anchor in a lens context', () => {
  const lens = { name: 'eng', write: { uid: 'uid-core', name: 'core' }, reads: [{ uid: 'uid-core', name: 'core' }, { uid: 'uid-docs', name: 'docs' }] } as any;
  const readSource = { repo: 'docs', id: 'docsid123456', branch: 'main' };
  const writeSource = { repo: 'core', id: 'coreid123456', branch: 'agent/main' };
  const READ_PATH = 'kb://docsid123456/kb/api/auth.md';

  const lensState = (factPath: string, factSource: any, asOf: AppState['asOf']): AppState => ({
    ...init, repo: 'core', branch: 'agent/main', headCommit: 'head1',
    context: { kind: 'lens', name: 'eng' }, lens, factPath, factSource, asOf,
  });

  const navFrom = (dispatch: ReturnType<typeof vi.fn>) =>
    dispatch.mock.calls.map(c => c[0]).find((a: any) => a.type === 'APPLY_NAV');

  it('returnToNow reads the open fact against its MOUNT repo/branch with the RELATIVE path', async () => {
    (api.fact as any).mockResolvedValue(mkFact('head1'));
    const dispatch = vi.fn();
    const { result } = renderHook(() =>
      useTimeTravel(lensState(READ_PATH, readSource, { mode: 'history', commit: 'c1' }), dispatch as any));
    await act(async () => { await result.current.returnToNow(); });
    expect(api.fact).toHaveBeenCalledWith('docs', 'main', 'kb/api/auth.md');
    // Navigates back to the RAW subject so RightPanel re-resolves through the lens.
    expect(navFrom(dispatch)?.factPath).toBe(READ_PATH);
  });

  // hopEdge does NOT read the target. It used to, at the HEAD endpoint, to
  // decide whether to stay live — and on success it discarded the edge's commit
  // and opened the target's current version. A reference resolves at the commit
  // it was added at, so there is nothing to classify and nothing to fetch.
  it('hopEdge pins to the edge commit from live, without reading the target', async () => {
    (api.fact as any).mockResolvedValue(mkFact('head1'));
    const dispatch = vi.fn();
    const { result } = renderHook(() =>
      useTimeTravel(lensState(READ_PATH, readSource, { mode: 'live' }), dispatch as any));
    act(() => { result.current.hopEdge('kb/other.md', 'pin1'); });
    expect(navFrom(dispatch)?.asOf).toEqual({ mode: 'history', commit: 'pin1' });
    // Scoped to THIS hop's target: api.fact is a module-level mock and other
    // tests in this file legitimately call it (returnToNow still reads).
    expect((api.fact as any).mock.calls.some((c: unknown[]) => c[2] === 'kb/other.md')).toBe(false);
  });

  it('write-repo fact: returnToNow anchors on the write mount, bare path passes through', async () => {
    (api.fact as any).mockResolvedValue(mkFact('head1'));
    const dispatch = vi.fn();
    const { result } = renderHook(() =>
      useTimeTravel(lensState('kb/ops/rollback.md', writeSource, { mode: 'history', commit: 'c1' }), dispatch as any));
    await act(async () => { await result.current.returnToNow(); });
    expect(api.fact).toHaveBeenCalledWith('core', 'agent/main', 'kb/ops/rollback.md');
  });

  // C2: an edge/in-body ref carries a MOUNT-RELATIVE bare path. In a lens context
  // a bare path canonically addresses the WRITE repo, so a hop from a non-write
  // read-mount fact must re-qualify the dispatched target to the SAME mount; a hop
  // from a write-repo fact (and repo context) keeps the bare path. The relative
  // path still drives the anchor read against the mount.
  it('hopEdge from a read-mount fact qualifies the dispatched target with the source mount id (C2)', async () => {
    (api.fact as any).mockResolvedValue(mkFact('head1'));
    const dispatch = vi.fn();
    const { result } = renderHook(() =>
      useTimeTravel(lensState(READ_PATH, readSource, { mode: 'live' }), dispatch as any));
    act(() => { result.current.hopEdge('kb/other.md', 'pin1'); });
    // The dispatched fact identity is qualified to the same read mount…
    expect(navFrom(dispatch)?.factPath).toBe('kb://docsid123456/kb/other.md');
    // …and anchored at the edge's commit.
    expect(navFrom(dispatch)?.asOf).toEqual({ mode: 'history', commit: 'pin1' });
  });

  it('hopEdge from a write-repo fact keeps the bare target (C2)', async () => {
    (api.fact as any).mockResolvedValue(mkFact('head1'));
    const dispatch = vi.fn();
    const { result } = renderHook(() =>
      useTimeTravel(lensState('kb/ops/rollback.md', writeSource, { mode: 'live' }), dispatch as any));
    act(() => { result.current.hopEdge('kb/other.md', 'pin1'); });
    expect(navFrom(dispatch)?.factPath).toBe('kb/other.md');
  });

  it('openFileAt from a read-mount fact qualifies with the source mount id (C2)', () => {
    const dispatch = vi.fn();
    const { result } = renderHook(() =>
      useTimeTravel(lensState(READ_PATH, readSource, { mode: 'live' }), dispatch as any));
    act(() => { result.current.openFileAt('kb/other.md', 'c9'); });
    expect(navFrom(dispatch)?.factPath).toBe('kb://docsid123456/kb/other.md');
  });
});
