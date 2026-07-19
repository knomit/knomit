import { describe, it, expect, vi } from 'vitest';
import { resolveLens } from './App';
import type { Action } from './state';
import type { Lens, RepoInfo } from './api';

const repos = (...names: string[]): RepoInfo[] => names.map(name => ({ name }));

describe('resolveLens — App-level lens resolution', () => {
  it('dispatches SET_LENS when the lens resolves', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const lens: Lens = { name: 'dev', write: 'work', reads: [{ repo: 'work' }] };
    await resolveLens('dev', repos('core', 'work'), dispatch, vi.fn().mockResolvedValue(lens));
    expect(actions).toEqual([{ type: 'SET_LENS', lens }]);
  });

  it('falls back to the first repo and surfaces a notice when the lens is gone', async () => {
    const actions: Action[] = [];
    const dispatch = (a: Action) => void actions.push(a);
    const getLens = vi.fn().mockRejectedValue(new Error('404 not found'));
    await resolveLens('deleted', repos('core', 'work'), dispatch, getLens);

    // A user-visible notice + a console error, then a fall back to the first repo.
    expect(actions.some(a => a.type === 'SET_NOTICE')).toBe(true);
    expect(actions.some(a => a.type === 'CONSOLE_LOG' && a.level === 'error')).toBe(true);
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
});
