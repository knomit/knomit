import { useCallback, useRef, useEffect } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { AppState, Action, AsOf } from './state';

export async function resolveHopAnchor(
  repo: string, branch: string, path: string, pinnedCommit: string,
  deps?: { fact?: typeof api.fact },
): Promise<{ asOf: AsOf }> {
  const factFn = deps?.fact ?? api.fact;
  // Classify the hop with a single HEAD read: we only need the target's current
  // commit (or a 404) to choose the anchor. RightPanel re-fetches the fact body
  // itself from the resulting asOf, so resolving it here would be a redundant
  // round-trip on every hop.
  try {
    const head = await factFn(repo, branch, path);            // no commit = HEAD endpoint
    if (head.commit_hash === pinnedCommit) {
      return { asOf: { mode: 'live' } };                      // target current
    }
    return { asOf: { mode: 'history', commit: pinnedCommit } }; // superseded
  } catch {
    // HEAD 404 -> retracted; pin to the referrer's commit. RightPanel's
    // ?fallback=before fetch surfaces the last-valid version there.
    return { asOf: { mode: 'history', commit: pinnedCommit } };
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

  // Monotonic navigation generation. The async navigations (hopEdge,
  // returnToNow) resolve an anchor over the network before dispatching; a slow
  // resolve must not clobber a newer navigation that started while it was in
  // flight. Every navigation — async or synchronous — bumps the counter, and
  // each async one only commits if it is still the latest. Without this, rapid
  // hops or a scrub-during-hop apply in resolution order, landing the user on a
  // stale subject.
  const navSeq = useRef(0);

  // hop:true so the reducer collapses cycles centrally — revisiting a fact
  // already in the trail unwinds to it instead of pushing a duplicate crumb.
  const hopEdge = useCallback(async (path: string, pinnedCommit: string) => {
    const seq = ++navSeq.current;
    const { asOf } = await resolveHopAnchor(repo, branch, path, pinnedCommit);
    if (seq !== navSeq.current) return;       // superseded by a newer navigation
    dispatch({ type: 'APPLY_NAV', view: 'library', factPath: path, asOf, hop: true });
  }, [repo, branch, dispatch]);

  const openFileAt = useCallback((path: string, commit: string) => {
    navSeq.current++;
    dispatch({ type: 'APPLY_NAV', view: 'library', factPath: path, asOf: { mode: 'history', commit }, hop: true });
  }, [dispatch]);

  const scrub = useCallback((commit: string, isLatest: boolean) => {
    navSeq.current++;
    const asOf: AsOf = isLatest ? { mode: 'live' } : { mode: 'history', commit };
    dispatch({ type: 'SET_AS_OF', asOf });
  }, [dispatch]);

  const returnToNow = useCallback(async () => {
    const seq = ++navSeq.current;
    const s = ref.current;
    const subject = s.factPath;
    if (!subject) { dispatch({ type: 'SET_AS_OF', asOf: { mode: 'live' } }); return; }
    const r = await computeReturnToNow(s.repo, s.branch, subject);
    if (seq !== navSeq.current) return;       // superseded by a newer navigation
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
