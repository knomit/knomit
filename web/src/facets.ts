// The view state: which page of the three-level shell is on screen.
//
// It is held by App with useReducer, and deliberately NOT on AppState. AppState
// feeds state.navStack, which is both the back stack and the time-travel
// trail, and a NavEntry captures a way of looking at facts. Nothing here is
// one: the rail being open, or the Settings page being the one on screen,
// changes what chrome and which page is shown, not which facts are listed or
// in what order. Putting it in AppState would make NAV_BACK decide what to do
// with it.
//
// This PR holds the rail, the facet and the connect lock. The server pages,
// the draft targets and the pinned settings-only targets join it in the PRs
// that build them.

import { useEffect, useReducer } from 'react';
import type { Dispatch } from 'react';
import { loadRailOpen, saveRailOpen, loadLastFacet, saveLastFacet } from './repoSelection';
import { repoAvailable, brokenLensMember } from './api';
import type { RepoInfo, Lens, RepoCreateStatus } from './api';
import type { BrowseContext } from './state';
import { activeCreateByRepo } from './useRepoCreates';

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

/** A page of a target. Code joins this only if the terminal ships. */
export type Facet = 'browse' | 'activity' | 'settings';

/** The order the tabs are drawn in, which is also the order facetsFor returns. */
const ALL_FACETS: Facet[] = ['browse', 'activity', 'settings'];

/**
 * facetsFor is the SINGLE authority on which facets (tabs) a target has. The
 * top bar draws exactly these, the facet rule below lands on Settings when the
 * current facet is not among them, and App renders a facet only if it is. A
 * tab a target lacks is omitted, never disabled.
 *
 * - An available repo with no create in flight: all three.
 * - A repo whose create is still working: Settings only. Browse would read a
 *   repository that is still being made (or removed), and Activity has nobody
 *   bound to it yet. Its tabs come back the moment the job is terminal.
 * - A lens whose mounts are all readable: all three.
 * - A lens with a mount that has no store: Activity and Settings. Every read
 *   under such a lens answers 503, so there is nothing to Browse; its sessions
 *   and its settings (where the broken mount is repaired) still exist.
 *
 * A repo that is listed but has no store, an archived repo and a create not yet
 * registered are not browse contexts at all; they arrive as pinned targets in
 * the next PR. Until then a listed repo with no store as the context (which
 * only a stale persisted value could produce) gets Settings alone, the safe
 * answer. A repo the list does not carry yet gets all three: the list lags a
 * create or a rename by one refresh.
 */
export function facetsFor(target: BrowseContext, repos: RepoInfo[], lenses: Lens[], creates: RepoCreateStatus[]): Facet[] {
  if (target.kind === 'repo') {
    const repo = repos.find(r => r.name === target.repo);
    // Not listed (yet): the list can lag a create or a rename by a refresh,
    // and the context's own reads will say if it is really gone.
    if (!repo) return ALL_FACETS;
    if (!repoAvailable(repo)) return ['settings'];
    if (activeCreateByRepo(creates).has(repo.name)) return ['settings'];
    return ALL_FACETS;
  }
  const lens = lenses.find(l => l.name === target.name);
  if (lens && brokenLensMember(lens, repos) !== null) return ['activity', 'settings'];
  return ALL_FACETS;
}

export interface ViewState {
  /** Whether the target rail is open. Persisted under knomit.rail. */
  railOpen: boolean;
  /** The facet of the current target. Persisted under knomit.facet. */
  facet: Facet;
  /** Set when a target without the current facet forced Settings: the facet
   *  to restore on the next target that has it. Never persisted. */
  lastFacet: Facet | null;
  /** One-shot: the Settings block to land on (the banner's "Reconnect…"). */
  focus: string | null;
  /** A connect commit is in flight. Its stream has no abort and no undo, and
   *  unmounting its page orphans it, so every way off the page refuses while
   *  this is set: rail rows and `+`, facet tabs, the gear. */
  locked: boolean;
}

export type ViewAction =
  | { type: 'TOGGLE_RAIL' }
  | { type: 'SET_RAIL'; open: boolean }
  /** A tab click (or a route that means one: Reconnect, entering a branch). */
  | { type: 'SET_FACET'; facet: Facet; focus?: string }
  /** A normal target was picked; `available` is facetsFor(that target). */
  | { type: 'SELECT_CONTEXT_TARGET'; available: Facet[] }
  | { type: 'SET_FOCUS'; focus: string }
  | { type: 'CLEAR_FOCUS' }
  | { type: 'SET_LOCKED'; locked: boolean };

/** initView reads the persisted parts of the view. Used as useReducer's init. */
export function initView(): ViewState {
  return { railOpen: loadRailOpen(), facet: loadLastFacet(), lastFacet: null, focus: null, locked: false };
}

export function viewReducer(s: ViewState, a: ViewAction): ViewState {
  switch (a.type) {
    case 'TOGGLE_RAIL':
      return { ...s, railOpen: !s.railOpen };
    case 'SET_RAIL':
      return s.railOpen === a.open ? s : { ...s, railOpen: a.open };
    case 'SET_FACET':
      // A deliberate choice of facet supersedes the remembered one: the next
      // target keeps what the reader just picked, not what they had before.
      return { ...s, facet: a.facet, lastFacet: null, focus: a.focus ?? null };
    case 'SELECT_CONTEXT_TARGET': {
      // The facet rule. A target that has the remembered facet gets it back.
      // Otherwise the current facet is kept if the target has it, and a target
      // that lacks it lands on Settings — which every target has — remembering
      // what the reader was on so the next normal target can restore it.
      if (s.lastFacet && a.available.includes(s.lastFacet)) {
        return { ...s, facet: s.lastFacet, lastFacet: null, focus: null };
      }
      if (a.available.includes(s.facet)) return s.focus === null ? s : { ...s, focus: null };
      return { ...s, lastFacet: s.lastFacet ?? s.facet, facet: 'settings', focus: null };
    }
    case 'SET_FOCUS':
      return { ...s, focus: a.focus };
    case 'CLEAR_FOCUS':
      return s.focus === null ? s : { ...s, focus: null };
    case 'SET_LOCKED':
      return s.locked === a.locked ? s : { ...s, locked: a.locked };
    default:
      return s;
  }
}

/** useViewState is App's view reducer plus its persistence: knomit.rail and
 *  knomit.facet are written whenever they change. Nothing else in the view is
 *  remembered across a reload. */
export function useViewState(): [ViewState, Dispatch<ViewAction>] {
  const [view, dispatch] = useReducer(viewReducer, undefined, initView);
  useEffect(() => { saveRailOpen(view.railOpen); }, [view.railOpen]);
  useEffect(() => { saveLastFacet(view.facet); }, [view.facet]);
  return [view, dispatch];
}
