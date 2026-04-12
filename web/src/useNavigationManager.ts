import { useRef, useEffect } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { AppState, Action, View } from './state';

export type NavRequest =
  | { view: View }                                                                              // mode-switch: manager fills gaps from current state
  | { view: 'tree' | 'chrono'; factPath: string | null }                                       // explicit fact selection in tree/chrono
  | { view: 'history'; historyCommit: string; factPath: string | null; factCommit?: string };  // explicit history navigation

/**
 * Pure async function — resolves a NavRequest to an APPLY_NAV dispatch.
 * Exported for unit testing without React.
 *
 * Mode-switch requests ({ view }) are resolved by reading current state:
 *   history → anchors to factCommit if known, else headCommit (fetches first file)
 *   tree    → preserves current factPath at HEAD
 *   chrono  → clears factPath (ChronoView will amend selection via AMEND_NAV)
 */
export async function resolveNavRequest(
  req: NavRequest,
  state: AppState,
  dispatch: Dispatch<Action>,
): Promise<void> {
  // ── Mode-switch: no explicit selection provided ────────────────────────────
  if (!('factPath' in req)) {
    if (req.view === 'history') {
      const { factPath, factCommit, headCommit } = state;
      if (factPath && factCommit) {
        // Already have a fact at a known commit — land there immediately.
        dispatch({ type: 'APPLY_NAV', view: 'history', historyCommit: factCommit, factPath, factCommit });
      } else if (headCommit) {
        // Land at HEAD; CommitPanel will auto-select the first file via AMEND_NAV once detail loads.
        dispatch({ type: 'APPLY_NAV', view: 'history', historyCommit: headCommit, factPath: null, factCommit: headCommit });
      } else {
        // headCommit not yet loaded — HistoryTimeline will amend selection once it fetches.
        dispatch({ type: 'APPLY_NAV', view: 'history', historyCommit: null, factPath: null, factCommit: null });
      }
    } else if (req.view === 'tree') {
      // Preserve current fact when already in tree/chrono (same HEAD context).
      // Clear it when coming from history — the selected fact was at a specific commit
      // and may not exist at HEAD.
      const factPath = state.view === 'history' ? null : state.factPath;
      dispatch({ type: 'APPLY_NAV', view: 'tree', historyCommit: null, factPath, factCommit: null });
    } else {
      // chrono: ChronoView will amend selection once it fetches recent facts.
      dispatch({ type: 'APPLY_NAV', view: 'chrono', historyCommit: null, factPath: null, factCommit: null });
    }
    return;
  }

  // ── Explicit history navigation ────────────────────────────────────────────
  if (req.view === 'history' && req.factPath === null) {
    // historyCommit given but factPath needs resolving (e.g. timeline click).
    try {
      const detail = await api.commitDetail(state.repo, state.branch, req.historyCommit);
      const first = (detail.files || [])[0];
      dispatch({ type: 'APPLY_NAV', view: 'history', historyCommit: req.historyCommit, factPath: first?.path ?? null, factCommit: req.historyCommit });
    } catch {
      dispatch({ type: 'APPLY_NAV', view: 'history', historyCommit: req.historyCommit, factPath: null, factCommit: req.historyCommit });
    }
  } else if (req.view === 'history') {
    // Fully specified: dispatch immediately.
    dispatch({ type: 'APPLY_NAV', view: 'history', historyCommit: req.historyCommit, factPath: req.factPath, factCommit: req.factCommit ?? req.historyCommit });
  } else {
    // tree or chrono with explicit factPath.
    dispatch({ type: 'APPLY_NAV', view: req.view, historyCommit: null, factPath: req.factPath, factCommit: null });
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
      dispatchRef.current({ type: 'APPLY_NAV', view: req.view, historyCommit: null, factPath: req.factPath, factCommit: null });
      return;
    }
    queue.current.push(req);
    drainRef.current();
  }).current;

  return { navigate };
}
