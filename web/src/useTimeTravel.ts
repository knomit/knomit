import { useCallback, useRef, useEffect } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { Fact } from './api';
import type { AppState, Action, AsOf } from './state';

export async function resolveHopAnchor(
  repo: string, branch: string, path: string, pinnedCommit: string,
  deps?: { fact?: typeof api.fact },
): Promise<{ asOf: AsOf; fact: Fact | null }> {
  const factFn = deps?.fact ?? api.fact;
  // HEAD read = present-check + the target's current last commit.
  try {
    const head = await factFn(repo, branch, path);            // no commit = HEAD endpoint
    if (head.commit_hash === pinnedCommit) {
      return { asOf: { mode: 'live' }, fact: head };           // target current
    }
    const pinned = await factFn(repo, branch, path, pinnedCommit, { fallback: 'before' });
    return { asOf: { mode: 'history', commit: pinnedCommit }, fact: pinned }; // superseded
  } catch {
    // HEAD 404 -> retracted; show the last-valid version at the pinned commit.
    const pinned = await factFn(repo, branch, path, pinnedCommit, { fallback: 'before' }).catch(() => null);
    return { asOf: { mode: 'history', commit: pinnedCommit }, fact: pinned };
  }
}

export type ReturnToNowResult =
  | { kind: 'subject'; factPath: string }
  | { kind: 'parent'; parentPath: string; notice: string };

export async function computeReturnToNow(
  repo: string, branch: string, subject: string,
  deps?: { fact?: typeof api.fact },
): Promise<ReturnToNowResult> {
  const factFn = deps?.fact ?? api.fact;
  try {
    await factFn(repo, branch, subject);          // HEAD read
    return { kind: 'subject', factPath: subject };
  } catch {
    const parts = subject.split('/');
    const parentPath = parts.length > 1 ? parts.slice(0, -1).join('/') : (parts[0] || '');
    return {
      kind: 'parent',
      parentPath,
      notice: `"${subject}" was retracted — no live version. Returned to now.`,
    };
  }
}

export function useTimeTravel(state: AppState, dispatch: Dispatch<Action>) {
  const ref = useRef(state);
  useEffect(() => { ref.current = state; }, [state]);
  const { repo, branch } = state;

  const hopEdge = useCallback(async (path: string, pinnedCommit: string) => {
    const { asOf } = await resolveHopAnchor(repo, branch, path, pinnedCommit);
    dispatch({ type: 'APPLY_NAV', view: 'library', factPath: path, asOf });
  }, [repo, branch, dispatch]);

  const openFileAt = useCallback((path: string, commit: string) => {
    dispatch({ type: 'APPLY_NAV', view: 'library', factPath: path, asOf: { mode: 'history', commit } });
  }, [dispatch]);

  const scrub = useCallback((commit: string, isLatest: boolean) => {
    const asOf: AsOf = isLatest ? { mode: 'live' } : { mode: 'history', commit };
    dispatch({ type: 'SET_AS_OF', asOf });
  }, [dispatch]);

  const returnToNow = useCallback(async () => {
    const s = ref.current;
    const subject = s.factPath;
    if (!subject) { dispatch({ type: 'SET_AS_OF', asOf: { mode: 'live' } }); return; }
    const r = await computeReturnToNow(s.repo, s.branch, subject);
    if (r.kind === 'subject') {
      dispatch({ type: 'APPLY_NAV', view: 'library', factPath: r.factPath, asOf: { mode: 'live' } });
    } else {
      dispatch({ type: 'APPLY_NAV', view: 'library', factPath: null,
        asOf: { mode: 'live' }, filters: [...s.filters.filter(f => f.category !== 'path'), { category: 'path', value: r.parentPath }] });
      dispatch({ type: 'SET_NOTICE', text: r.notice });
    }
  }, [dispatch]);

  return { hopEdge, openFileAt, scrub, returnToNow };
}
