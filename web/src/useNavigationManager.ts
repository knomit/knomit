import { useRef } from 'react';
import type { Dispatch } from 'react';
import type { AppState, Action } from './state';

export type NavRequest =
  | { view: 'library' }
  | { view: 'library'; factPath: string | null };

/**
 * Pure async function — resolves a NavRequest to an APPLY_NAV dispatch.
 * Exported for unit testing without React.
 *
 * Mode-switch requests ({ view: 'library' }) preserve the open fact and
 * demote any non-live anchor to live (Library is HEAD-only by design).
 *
 * Explicit selection requests ({ view: 'library'; factPath }) force a live
 * anchor regardless of the current asOf.
 */
export async function resolveNavRequest(
  req: NavRequest,
  state: AppState,
  dispatch: Dispatch<Action>,
): Promise<void> {
  if (!('factPath' in req)) {
    // Mode-switch: preserve the open fact, demote any non-live anchor to live
    // (Library is HEAD-only by design — listFacts, listTopics, search are all HEAD).
    dispatch({ type: 'APPLY_NAV', view: 'library', factPath: state.factPath, asOf: { mode: 'live' } });
    return;
  }
  // Explicit selection — force live anchor.
  dispatch({ type: 'APPLY_NAV', view: 'library', factPath: req.factPath, asOf: { mode: 'live' } });
}

/**
 * React hook — synchronous wrapper around resolveNavRequest.
 * Uses stale-ref pattern so callbacks always read current state.
 */
export function useNavigationManager(
  state: AppState,
  dispatch: Dispatch<Action>,
): { navigate: (req: NavRequest) => void } {
  const stateRef = useRef(state);
  const dispatchRef = useRef(dispatch);
  stateRef.current = state;
  dispatchRef.current = dispatch;

  const navigate = useRef((req: NavRequest) => {
    resolveNavRequest(req, stateRef.current, dispatchRef.current);
  }).current;

  return { navigate };
}
