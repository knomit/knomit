import { useCallback, useRef, useEffect } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import { factHistoryAnchor } from './state';
import type { AppState, Action, AsOf } from './state';

// qualifyHopTarget re-qualifies an edge/in-body ref target for the fact-open
// dispatch in a lens context. Connections-panel groups and in-body refs carry
// MOUNT-RELATIVE bare paths; a bare path is canonically the lens WRITE repo, so
// dispatching it verbatim from a NON-write read-mount fact would 404 (or open the
// write repo's shadow copy). When the open fact lives in a non-write mount,
// re-qualify a bare target to that SAME mount (kb://<sourceId>/<relPath>) so a
// same-mount hop lands where the referrer lives. Write-repo facts and repo
// context keep the bare path; an already-qualified kb:// target (a cross-mount
// ref naming another mount — the accepted pre-existing gap) passes through
// untouched. The RELATIVE path is still what resolveHopAnchor reads against the
// mount; only the dispatched fact identity is qualified.
export function qualifyHopTarget(s: AppState, path: string): string {
  if (s.context.kind !== 'lens') return path;
  const src = s.factSource;
  if (!src || src.repo === s.lens?.write.name) return path;
  if (path.startsWith('kb://')) return path;
  return `kb://${src.id}/${path}`;
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
  // returnToNow still resolves its anchor over the network and reads against the
  // OPEN FACT's source mount rather than the browse surface — see
  // computeReturnToNow, which takes that mount explicitly. hopEdge needs no
  // mount at all any more: the edge already carries the commit to address.

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
  /**
   * Follow an edge to its target, AT THE COMMIT THE EDGE RECORDS.
   *
   * `pinnedCommit` is the edge's target_commit — the version of the target the
   * referrer reasoned over. It is the anchor unconditionally: a reference
   * resolves at the commit it was added at, whether or not the app is currently
   * live and whether or not the target has moved since
   * (kb/principles/philosophy/historical-not-current). If that commit happens to
   * be the target's current tip, fine — that is incidental, not a decision made
   * here.
   *
   * This used to await resolveHopAnchor, which read the target at the HEAD
   * endpoint while live and, on 200, discarded pinnedCommit and opened whatever
   * the target is NOW — silently re-pointing a synthesis at evidence it never
   * saw. The function is gone: there is nothing to resolve, so the hop is
   * synchronous and costs no round-trip.
   *
   * The empty-commit case is not a HEAD fallback in disguise. An edge with no
   * recorded target_commit offers nothing to address; live is the only
   * reachable answer, and the honest one.
   */
  const hopEdge = useCallback((path: string, pinnedCommit: string) => {
    navSeq.current++;
    const asOf: AsOf = pinnedCommit
      ? { mode: 'history', commit: pinnedCommit }
      : { mode: 'live' };
    // Qualify the dispatched fact identity to the referrer's mount (lens read
    // mount) so RightPanel re-resolves it there.
    dispatch({ type: 'APPLY_NAV', view: 'library', factPath: qualifyHopTarget(ref.current, path), asOf, hop: true });
  }, [dispatch]);

  const openFileAt = useCallback((path: string, commit: string) => {
    navSeq.current++;
    dispatch({ type: 'APPLY_NAV', view: 'library', factPath: qualifyHopTarget(ref.current, path), asOf: { mode: 'history', commit }, hop: true });
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
    const a = factHistoryAnchor(s, subject);
    const r = await computeReturnToNow(a.repo, a.branch, a.path);
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
