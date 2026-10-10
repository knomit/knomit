// The view state: which page of the three-level shell is on screen.
//
// It is held by App with useReducer, and deliberately NOT on AppState. AppState
// feeds state.navStack, which is both the back stack and the time-travel
// trail, and a NavEntry captures a way of looking at facts. Nothing here is
// one: the rail being open changes what chrome is shown, not which facts are
// listed or in what order. Putting it in AppState would make NAV_BACK decide
// what to do with it.
//
// This PR holds only the rail. The facet, the server pages, the draft targets
// and the pinned settings-only targets join it in the PRs that build them.

import { loadRailOpen } from './repoSelection';
import type { RepoInfo } from './api';

/** remoteFailing: the listing says this repo's last sync or last push failed.
 *  An absent `remote` (no origin, no store, older server) is nothing to show. */
export function remoteFailing(r: Pick<RepoInfo, 'remote'>): boolean {
  return r.remote?.sync === 'error' || r.remote?.push === 'error';
}

/** otherRepoFailing: some repo OTHER than `current` has a failing remote. The
 *  rail toggle's dot. The current repo is excluded because its failure already
 *  has the app banner. */
export function otherRepoFailing(repos: RepoInfo[], current: string): boolean {
  return repos.some(r => r.name !== current && remoteFailing(r));
}

export interface ViewState {
  /** Whether the target rail is open. Persisted under knomit.rail. */
  railOpen: boolean;
}

export type ViewAction =
  | { type: 'TOGGLE_RAIL' }
  | { type: 'SET_RAIL'; open: boolean };

/** initView reads the persisted parts of the view. Used as useReducer's init. */
export function initView(): ViewState {
  return { railOpen: loadRailOpen() };
}

export function viewReducer(s: ViewState, a: ViewAction): ViewState {
  switch (a.type) {
    case 'TOGGLE_RAIL':
      return { ...s, railOpen: !s.railOpen };
    case 'SET_RAIL':
      return s.railOpen === a.open ? s : { ...s, railOpen: a.open };
    default:
      return s;
  }
}
