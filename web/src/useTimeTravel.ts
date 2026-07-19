import { useCallback, useRef, useEffect } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import { openFactSource } from './state';
import { displayLensPath } from './utils';
import type { AppState, Action, AsOf } from './state';

export async function resolveHopAnchor(
  repo: string, branch: string, path: string, pinnedCommit: string,
  fromAsOf: AsOf,
  deps?: { fact?: typeof api.fact },
): Promise<{ asOf: AsOf }> {
  const factFn = deps?.fact ?? api.fact;
  // Already in a history/diff excursion: keep time-travelling. The hop stays
  // anchored at the edge's commit so the target is shown as the referrer saw
  // it — no HEAD read is needed to make that choice.
  if (fromAsOf.mode !== 'live') {
    return { asOf: { mode: 'history', commit: pinnedCommit } };
  }
  // Following a ref while live shows the target's LIVE version — even if the
  // target has changed since the edge was formed (it is still live). The only
  // reason to leave live is a retracted target: a single HEAD read tells live
  // (200) from retracted (404), and on 404 we pin to the edge's commit so
  // RightPanel's ?fallback=before fetch surfaces the last-valid version.
  try {
    await factFn(repo, branch, path);                         // no commit = HEAD endpoint
    return { asOf: { mode: 'live' } };
  } catch {
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
  // Temporal reads anchor on the OPEN FACT's source mount, not the browse
  // surface: {state.repo, state.branch} in a repo context (unchanged), the read
  // mount the open fact came from in a lens context. Same-mount edge/subject
  // reads then resolve via that mount's repo-scoped endpoints.
  const { repo, branch } = openFactSource(state);

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
    // Capture the mode at click time: a live hop stays live, a hop from within a
    // history excursion stays in history.
    const fromAsOf = ref.current.asOf;
    const { asOf } = await resolveHopAnchor(repo, branch, path, pinnedCommit, fromAsOf);
    if (seq !== navSeq.current) return;       // superseded by a newer navigation
    dispatch({ type: 'APPLY_NAV', view: 'library', factPath: path, asOf, hop: true });
  }, [repo, branch, dispatch]);

  const openFileAt = useCallback((path: string, commit: string) => {
    navSeq.current++;
    dispatch({ type: 'APPLY_NAV', view: 'library', factPath: path, asOf: { mode: 'history', commit }, hop: true });
  }, [dispatch]);

  // Scrubbing always means "view this version in history". Selecting a version
  // — even the newest — keeps the history excursion open; returning to live is
  // returnToNow's job, never a side effect of picking a version.
  const scrub = useCallback((commit: string) => {
    navSeq.current++;
    dispatch({ type: 'SET_AS_OF', asOf: { mode: 'history', commit } });
  }, [dispatch]);

  const returnToNow = useCallback(async () => {
    const seq = ++navSeq.current;
    const s = ref.current;
    const subject = s.factPath;
    if (!subject) { dispatch({ type: 'SET_AS_OF', asOf: { mode: 'live' } }); return; }
    // Read the subject against its source mount with the RELATIVE path; dispatch
    // still carries the RAW subject so a lens read-mount fact re-resolves through
    // getLensFact (the raw kb://<id12>/ address is the fact's canonical identity).
    const src = openFactSource(s);
    const r = await computeReturnToNow(src.repo, src.branch, displayLensPath(subject));
    if (seq !== navSeq.current) return;       // superseded by a newer navigation
    if (r.kind === 'subject') {
      dispatch({ type: 'APPLY_NAV', view: 'library', factPath: subject, asOf: { mode: 'live' } });
    } else {
      dispatch({ type: 'APPLY_NAV', view: 'library', factPath: null,
        asOf: { mode: 'live' }, filters: [...s.filters.filter(f => f.category !== 'path'), { category: 'path', value: r.parentPath }] });
      dispatch({ type: 'SET_NOTICE', text: r.notice });
    }
  }, [dispatch]);

  return { hopEdge, openFileAt, scrub, returnToNow };
}
