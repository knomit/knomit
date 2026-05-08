import { useRef, useEffect } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { AppState, Action, View, AsOf } from './state';
import { selectAnchorCommit } from './state';

export type NavRequest =
  | { view: View }                                                                 // mode-switch: manager fills gaps from current state
  | { view: 'tree' | 'chrono'; factPath: string | null }                          // explicit fact selection in tree/chrono
  | { view: 'history'; asOf: AsOf; factPath: string | null };                     // explicit history navigation

/**
 * Pure async function — resolves a NavRequest to an APPLY_NAV dispatch.
 * Exported for unit testing without React.
 *
 * Mode-switch requests ({ view }) are resolved by reading current state:
 *   history → anchors to current asOf if scrubbed/diff, else most recent commit
 *             that touches the open fact (via /commits), else headCommit.
 *   tree    → preserves current factPath (carries asOf forward).
 *   chrono  → clears factPath (ChronoView will amend selection via AMEND_NAV).
 */
export async function resolveNavRequest(
  req: NavRequest,
  state: AppState,
  dispatch: Dispatch<Action>,
): Promise<void> {
  // ── Mode-switch: no explicit selection provided ────────────────────────────
  if (!('factPath' in req)) {
    if (req.view === 'history') {
      const { factPath, headCommit } = state;
      const anchorAtClick = selectAnchorCommit(state);
      if (factPath) {
        // Resolve the fact's most-recent commit so CommitPanel opens a commit whose
        // file list actually contains factPath.
        try {
          const hist = await api.history(state.repo, state.branch, factPath);
          const lastTouched = hist.entries[0]?.commit ?? anchorAtClick ?? headCommit ?? null;
          if (lastTouched) {
            dispatch({ type: 'APPLY_NAV', view: 'history', factPath, asOf: { mode: 'scrubbed', commit: lastTouched } });
          } else {
            dispatch({ type: 'APPLY_NAV', view: 'history', factPath, asOf: { mode: 'live' } });
          }
        } catch {
          const fallback = anchorAtClick ?? headCommit ?? null;
          dispatch({
            type: 'APPLY_NAV',
            view: 'history',
            factPath,
            asOf: fallback ? { mode: 'scrubbed', commit: fallback } : { mode: 'live' },
          });
        }
      } else if (headCommit) {
        // Land at HEAD; CommitPanel will auto-select the first file via AMEND_NAV once detail loads.
        dispatch({ type: 'APPLY_NAV', view: 'history', factPath: null, asOf: { mode: 'scrubbed', commit: headCommit } });
      } else {
        // headCommit not yet loaded — HistoryTimeline will amend selection once it fetches.
        dispatch({ type: 'APPLY_NAV', view: 'history', factPath: null, asOf: { mode: 'live' } });
      }
    } else if (req.view === 'tree') {
      // Preserve current fact when already in tree/chrono (same anchor context).
      // Clear it when coming from history — the selected fact was at a specific commit
      // and may not exist at the current anchor.
      const factPath = state.view === 'history' ? null : state.factPath;
      dispatch({ type: 'APPLY_NAV', view: 'tree', factPath, asOf: state.asOf });
    } else {
      // chrono: ChronoView will amend selection once it fetches recent facts.
      dispatch({ type: 'APPLY_NAV', view: 'chrono', factPath: null, asOf: state.asOf });
    }
    return;
  }

  // ── Explicit history navigation ────────────────────────────────────────────
  if (req.view === 'history' && req.factPath === null) {
    const targetCommit = req.asOf.mode === 'scrubbed' ? req.asOf.commit
                       : req.asOf.mode === 'diff'     ? req.asOf.to
                       : state.headCommit;
    if (!targetCommit) {
      dispatch({ type: 'APPLY_NAV', view: 'history', factPath: null, asOf: req.asOf });
      return;
    }
    try {
      const detail = await api.commitDetail(state.repo, state.branch, targetCommit);
      const first = (detail.files || [])[0];
      dispatch({ type: 'APPLY_NAV', view: 'history', factPath: first?.path ?? null, asOf: req.asOf });
    } catch {
      dispatch({ type: 'APPLY_NAV', view: 'history', factPath: null, asOf: req.asOf });
    }
  } else if (req.view === 'history') {
    // Fully specified: dispatch immediately.
    dispatch({ type: 'APPLY_NAV', view: 'history', factPath: req.factPath, asOf: req.asOf });
  } else {
    // tree or chrono with explicit factPath — carry current asOf forward.
    dispatch({ type: 'APPLY_NAV', view: req.view, factPath: req.factPath, asOf: state.asOf });
  }
}

/**
 * React hook that wraps resolveNavRequest in a serial queue.
 * Uses stale-ref pattern so async callbacks always read current state.
 */
export function useNavigationManager(
  state: AppState,
  dispatch: Dispatch<Action>,
): { navigate: (req: NavRequest) => void } {
  const stateRef = useRef(state);
  const dispatchRef = useRef(dispatch);
  // Direct assignment in render body keeps refs current without async effect delay.
  stateRef.current = state;
  dispatchRef.current = dispatch;

  const queue = useRef<NavRequest[]>([]);
  const processing = useRef(false);
  const drainRef = useRef<() => void>(() => {});

  // Defined once — all captured values are refs so they're always current.
  useEffect(() => {
    drainRef.current = function drain() {
      if (processing.current) return;
      const req = queue.current.shift();
      if (!req) return;
      processing.current = true;
      resolveNavRequest(req, stateRef.current, dispatchRef.current)
        .finally(() => {
          processing.current = false;
          drainRef.current();
        });
    };
  }, []);

  // Stable wrapper: identity never changes, always dispatches through current ref.
  // Simple tree/chrono factPath navigations dispatch synchronously so each arrow-key
  // press gets its own dispatch; React chains the reducer calls and navStack is correct.
  // History navigation (needs async commitDetail fetch) still goes through the queue.
  const navigate = useRef((req: NavRequest) => {
    if ('factPath' in req && req.view !== 'history') {
      dispatchRef.current({
        type: 'APPLY_NAV',
        view: req.view,
        factPath: req.factPath,
        asOf: stateRef.current.asOf,
      });
      return;
    }
    queue.current.push(req);
    drainRef.current();
  }).current;

  return { navigate };
}
