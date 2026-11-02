import { describe, expect, it, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import { useListUrl } from './listUrl';
import { init } from './state';
import type { AppState } from './state';
import type { RepoInfo, Status } from './api';

// #713 review L2, the restore half: the branch status useListUrl asks for on
// a reload must not land once the user has left the URL's repo (the restore is
// abandoned then, and SET_STATUS has no repo guard).
//
// Sabotage: drop the stillMine() check in the .then: onStatus is called with
// exp/try after the switch to beta.

let releaseStatus: (s: Status) => void = () => {};
vi.mock('./api', () => ({
  api: { status: vi.fn(() => new Promise<Status>(res => { releaseStatus = res; })) },
}));

const repos = [{ name: 'alpha', uid: 'u-a' }, { name: 'beta', uid: 'u-b' }] as RepoInfo[];
const at = (repo: string, branch: string): AppState => ({ ...init, repo, branch, context: { kind: 'repo', repo } });

describe('useListUrl branch entry', () => {
  it('a late status for the repo the user left is dropped', async () => {
    window.history.replaceState(null, '', '/?ctx_repo=u-a&ctx_branch=exp%2Ftry&l_q=x');
    const onStatus = vi.fn();
    const dispatch = vi.fn();
    const { result, rerender } = renderHook(({ s }) => useListUrl(s, dispatch, repos, onStatus), { initialProps: { s: at('alpha', 'machine/test') } });
    act(() => result.current.adopt('alpha'));
    rerender({ s: at('alpha', 'machine/test') });
    const { api } = await import('./api');
    expect(api.status).toHaveBeenCalledWith('alpha', 'exp/try');

    // The user picks beta while the status is in flight.
    rerender({ s: at('beta', 'machine/test') });
    await act(async () => { releaseStatus({ branch: 'exp/try' } as Status); await Promise.resolve(); });

    expect(onStatus).not.toHaveBeenCalled();
    expect(dispatch).not.toHaveBeenCalledWith(expect.objectContaining({ type: 'RESTORE_LIST' }));
    // Abandoned: written from beta's own state, the stale list gone.
    expect(new URLSearchParams(window.location.search).get('ctx_repo')).toBe('u-b');
    expect(new URLSearchParams(window.location.search).get('l_q')).toBeNull();
  });

  it('the same status lands while the restore is still in progress', async () => {
    window.history.replaceState(null, '', '/?ctx_repo=u-a&ctx_branch=exp%2Ftry');
    const onStatus = vi.fn();
    const { result, rerender } = renderHook(({ s }) => useListUrl(s, vi.fn(), repos, onStatus), { initialProps: { s: at('alpha', 'machine/test') } });
    act(() => result.current.adopt('alpha'));
    rerender({ s: at('alpha', 'machine/test') });
    await act(async () => { releaseStatus({ branch: 'exp/try' } as Status); await Promise.resolve(); });
    expect(onStatus).toHaveBeenCalledWith({ branch: 'exp/try' });
  });
});
