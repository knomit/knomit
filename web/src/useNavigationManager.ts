import { useRef, useEffect } from 'react';
import type { Dispatch } from 'react';
import { api } from './api';
import type { AppState, Action } from './state';

export type NavRequest =
  | { view: 'tree' | 'chrono'; factPath: string | null }
  | { view: 'history'; historyCommit: string; factPath: string | null; factCommit?: string };

/**
 * Pure async function — resolves a NavRequest to an APPLY_NAV dispatch.
 * Exported for unit testing without React.
 */
export async function resolveNavRequest(
  req: NavRequest,
  repo: string,
  dispatch: Dispatch<Action>,
): Promise<void> {
  if (req.view === 'history' && req.factPath === null) {
    // Partial spec: need commitDetail to discover the first file
    try {
      const detail = await api.commitDetail(repo, req.historyCommit);
      const first = (detail.files || [])[0];
      dispatch({
        type: 'APPLY_NAV',
        view: 'history',
        historyCommit: req.historyCommit,
        factPath: first?.path ?? null,
        factCommit: req.historyCommit,
      });
    } catch {
      // Graceful degradation: land in history mode with no fact selected
      dispatch({
        type: 'APPLY_NAV',
        view: 'history',
        historyCommit: req.historyCommit,
        factPath: null,
        factCommit: req.historyCommit,
      });
    }
  } else if (req.view === 'history') {
    // Fully specified: dispatch immediately
    dispatch({
      type: 'APPLY_NAV',
      view: 'history',
      historyCommit: req.historyCommit,
      factPath: req.factPath,
      factCommit: req.factCommit ?? req.historyCommit,
    });
  } else {
    // tree or chrono
    dispatch({
      type: 'APPLY_NAV',
      view: req.view,
      historyCommit: null,
      factPath: req.factPath,
      factCommit: null,
    });
  }
}

/**
 * React hook that wraps resolveNavRequest in a serial queue.
 * Uses stale-ref pattern so async callbacks always read current state.repo.
 */
export function useNavigationManager(
  state: AppState,
  dispatch: Dispatch<Action>,
): { navigate: (req: NavRequest) => void } {
  const stateRef = useRef(state);
  const dispatchRef = useRef(dispatch);

  useEffect(() => {
    stateRef.current = state;
    dispatchRef.current = dispatch;
  });

  const queue = useRef<NavRequest[]>([]);
  const processing = useRef(false);

  const drainRef = useRef<() => void>(() => {});
  const navigateRef = useRef<(req: NavRequest) => void>(() => {});

  useEffect(() => {
    drainRef.current = function drain() {
      if (processing.current) return;
      const req = queue.current.shift();
      if (!req) return;
      processing.current = true;
      resolveNavRequest(req, stateRef.current.repo, dispatchRef.current)
        .finally(() => {
          processing.current = false;
          drainRef.current();
        });
    };

    navigateRef.current = function navigate(req: NavRequest) {
      queue.current.push(req);
      drainRef.current();
    };
  });

  // Stable wrapper: identity never changes, always dispatches through current ref
  const navigate = useRef((req: NavRequest) => navigateRef.current(req)).current;

  return { navigate };
}
