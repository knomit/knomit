import { useRef } from 'react';
import type { Dispatch } from 'react';
import type { AppState, Action } from './state';
import { displayLensPath } from './utils';

export type NavRequest =
  | { view: 'library' }
  | { view: 'library'; factPath: string | null }
  /** Open the fact AND take the tree to where it lives — see resolveNavRequest. */
  | { view: 'library'; factPath: string; reveal: true };

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
  if ('reveal' in req && req.reveal) {
    // Reveal: arrive as though you had browsed there. Opening a highlight used
    // to set the fact and leave the left panel wherever it was — usually the
    // ontology root — so you landed on a fact with no idea where in the tree it
    // lived, and no folder around it to look at next.
    //
    // ONE dispatch, so it is one entry in the back stack: a reveal is a single
    // move, and Back should undo the whole of it rather than the fact and the
    // folder separately.
    //
    // The path chip carries the DISPLAY path (kb://<id12>/ stripped): the tree
    // browses ontology paths, and a qualified prefix would match nothing. The
    // factPath keeps its qualified form — that is the fact's identity, and the
    // tree row that selects it carries the same one.
    //
    // EVERY other chip goes, and this is load-bearing rather than tidiness.
    // Library demotes Path to Recent whenever a content chip is set, because the
    // topics endpoint takes no content filters and a chip above a tree is a chip
    // that does nothing. So a reveal that kept its chips would ask for 'path',
    // be demoted straight back to 'recent', and land the reader in a filtered
    // flat list instead of the folder — and a highlight comes from path-scoped
    // stats, so the fact just opened need not match the chip at all. Asking for
    // the tree means asking for the one mode the chips cannot survive.
    const dir = parentDir(displayLensPath(req.factPath));
    dispatch({
      type: 'APPLY_NAV', view: 'library', factPath: req.factPath, asOf: { mode: 'live' },
      filters: [{ category: 'path', value: dir }],
      sort: 'path',
    });
    return;
  }
  // Explicit selection — force live anchor.
  dispatch({ type: 'APPLY_NAV', view: 'library', factPath: req.factPath, asOf: { mode: 'live' } });
}

/** The folder a fact lives in. A fact directly under the ontology root scopes
 *  to the root itself rather than to an empty string, which would read as "no
 *  path chip" and send the tree somewhere else entirely. */
function parentDir(path: string): string {
  const cut = path.lastIndexOf('/');
  return cut > 0 ? path.slice(0, cut) : path;
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
