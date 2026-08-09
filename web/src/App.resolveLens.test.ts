import { describe, it, expect, vi } from 'vitest';
import { resolveLens, refreshContextAfterChange } from './App';
import type { Action } from './state';
import type { Lens, RepoInfo } from './api';

const repos = (...names: string[]): RepoInfo[] => names.map(name => ({ name, uid: `uid-${name}` }));

describe('resolveLens — App-level lens resolution', () => {
  it('dispatches SET_LENS when the lens resolves', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const lens: Lens = { name: 'dev', write: { uid: 'uid-work', name: 'work' }, reads: [{ uid: 'uid-work', name: 'work' }] };
    await resolveLens('dev', repos('core', 'work'), dispatch, vi.fn().mockResolvedValue(lens));
    expect(actions).toEqual([{ type: 'SET_LENS', lens }]);
  });

  it('falls back to the first repo and surfaces a notice when the lens is gone', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const getLens = vi.fn().mockRejectedValue(new Error('404 not found'));
    await resolveLens('deleted', repos('core', 'work'), dispatch, getLens);

    // A user-visible notice, then a fall back to the first repo. The paired
    // developer line goes to the browser console (App's `diag`), not through
    // dispatch — a failed resolve must not be reported to a user twice.
    expect(actions.some(a => a.type === 'SET_NOTICE')).toBe(true);
    expect(actions).toContainEqual({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'core' } });
    // Never dispatches SET_LENS on failure.
    expect(actions.some(a => a.type === 'SET_LENS')).toBe(false);
  });

  it('surfaces the notice but does not fall back when no repos exist', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    await resolveLens('deleted', repos(), dispatch, vi.fn().mockRejectedValue(new Error('gone')));
    expect(actions.some(a => a.type === 'SET_NOTICE')).toBe(true);
    expect(actions.some(a => a.type === 'SET_CONTEXT')).toBe(false);
  });

  // I3: a slow failing resolve for lens A must not yank the user out of lens B
  // (or a repo) they've since switched to. The guard suppresses the whole
  // fallback (no notice, no console, no context change) when A is no longer current.
  it('does not fall back when the lens context drifted while the resolve was in flight (I3)', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const getLens = vi.fn().mockRejectedValue(new Error('slow 404'));
    await resolveLens('A', repos('core', 'work'), dispatch, getLens, () => false);
    expect(actions).toEqual([]);
  });

  it('still falls back when the failing lens is the current one (guard passes)', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    await resolveLens('A', repos('core'), dispatch, vi.fn().mockRejectedValue(new Error('gone')), () => true);
    expect(actions).toContainEqual({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'core' } });
  });
});

// I4: after the RepoManager reports a create/delete/edit, re-sync the browse
// surface. A mutated active lens re-resolves (refreshing state.lens); a deleted
// browsed lens falls back with the notice; a repo context switches off a removed
// active repo.
describe('refreshContextAfterChange — post-mutation resync', () => {
  it('re-resolves the active lens so edited mounts refresh state.lens (I4)', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const edited: Lens = { name: 'eng', write: { uid: 'uid-core', name: 'core' }, reads: [{ uid: 'uid-core', name: 'core' }, { uid: 'uid-docs', name: 'docs' }, { uid: 'uid-infra', name: 'infra' }] };
    const getLens = vi.fn().mockResolvedValue(edited);
    await refreshContextAfterChange(dispatch, { kind: 'lens', name: 'eng' }, 'core', {
      listLenses: vi.fn().mockResolvedValue([edited]),
      repos: vi.fn().mockResolvedValue(repos('core', 'docs', 'infra')),
      getLens,
    });
    expect(getLens).toHaveBeenCalledWith('eng');
    expect(actions).toContainEqual({ type: 'SET_LENS', lens: edited });
    // No fallback/notice while the lens still exists.
    expect(actions.some(a => a.type === 'SET_CONTEXT')).toBe(false);
    expect(actions.some(a => a.type === 'SET_NOTICE')).toBe(false);
  });

  it('falls back with a notice when the browsed lens was deleted (I4)', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const getLens = vi.fn();
    await refreshContextAfterChange(dispatch, { kind: 'lens', name: 'gone' }, 'core', {
      listLenses: vi.fn().mockResolvedValue([]), // no longer listed
      repos: vi.fn().mockResolvedValue(repos('core', 'work')),
      getLens,
    });
    expect(actions.some(a => a.type === 'SET_NOTICE')).toBe(true);
    expect(actions).toContainEqual({ type: 'SET_CONTEXT', context: { kind: 'repo', repo: 'core' } });
    // A deleted lens isn't in the list → no wasted getLens round-trip.
    expect(getLens).not.toHaveBeenCalled();
  });

  it('switches off an archived active repo in a repo context (existing behavior)', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    await refreshContextAfterChange(dispatch, { kind: 'repo', repo: 'gone' }, 'gone', {
      listLenses: vi.fn().mockResolvedValue([]),
      repos: vi.fn().mockResolvedValue(repos('core', 'work')),
    });
    expect(actions).toContainEqual({ type: 'SET_REPO', repo: 'core' });
  });

  it('clears the repo context when the last repo is archived', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    await refreshContextAfterChange(dispatch, { kind: 'repo', repo: 'only' }, 'only', {
      listLenses: vi.fn().mockResolvedValue([]),
      repos: vi.fn().mockResolvedValue([]),
    });
    // state.repo/state.branch key the SSE subscription. Leaving them on the repo
    // that was just archived holds an EventSource open against a route that now
    // 404s, and EventSource retries that forever.
    expect(actions).toContainEqual({ type: 'SET_REPO', repo: '' });
  });

  it('clears the repo context from a lens surface too when no repos remain', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const getLens = vi.fn();
    await refreshContextAfterChange(dispatch, { kind: 'lens', name: 'eng' }, 'only', {
      listLenses: vi.fn().mockResolvedValue([{ name: 'eng', write: { uid: 'uid-only', name: 'only' }, reads: [] }]),
      repos: vi.fn().mockResolvedValue([]),
      getLens,
    });
    expect(actions).toContainEqual({ type: 'SET_REPO', repo: '' });
    // A lens over zero repos has nothing to resolve, and the app is showing the
    // zero-repo screen regardless — the round-trip would be pure waste.
    expect(getLens).not.toHaveBeenCalled();
  });
});
