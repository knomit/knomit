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
// It holds the rail, the facet, the connect lock, the pinned settings-only
// targets, the draft and the server pages behind the gear.

import { useEffect, useReducer } from 'react';
import type { Dispatch } from 'react';
import { loadRailOpen, saveRailOpen, loadLastFacet, saveLastFacet } from './repoSelection';
import { repoAvailable, brokenLensMember } from './api';
import type { RepoInfo, Lens, RepoCreateStatus, ArchivedRepo, FleetStatus } from './api';
import type { BrowseContext } from './state';
import { activeCreateByRepo, createFlag } from './useRepoCreates';

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

/** A page of a target. Code joins this only if the terminal ships. The
 *  fleet target's first page is Members (its Browse); no other target has it. */
export type Facet = 'browse' | 'activity' | 'settings' | 'members';

/**
 * A target that can NOT be the browse context, because it has no store to read:
 * an archived repo, a create that is not registered yet, a registered repo whose
 * store did not open, a lens with a mount that has no store. It is pinned OVER
 * the context, which stays where it was, so leaving it lands back on the
 * context with nothing to restore.
 *
 * A broken lens is pinned rather than entered because every read under it
 * answers 503 (a lens binds all its members or none), and the context switch
 * would fire those reads. Its Settings is still the only place the broken mount
 * can be removed: the server refuses to archive a repo a lens mounts.
 */
export type PinnedTarget =
  | { kind: 'archived'; id: string }
  | { kind: 'pending'; createId: string }
  | { kind: 'unavailable'; repo: string }
  | { kind: 'brokenLens'; name: string }
  /** The fleet this instance is in: a target of its own (Members, Activity,
   *  Settings), shown over the context like every other pinned target,
   *  because it reads the fleet routes, never the browse context's store. */
  | { kind: 'fleet' };

export type DraftKind = 'repo' | 'lens' | 'fleet';

/** A server page behind the gear. Not a target: nothing on it belongs to a
 *  repository, a lens or the fleet. */
export type ServerPage = 'logs' | 'auth';

/** Anything the rail can select: a normal target (the browse context), a
 *  pinned one, or a draft. */
export type Target = BrowseContext | PinnedTarget | { kind: 'draft'; draft: DraftKind };

/** Where Cancel on a draft goes back to. */
export interface ReturnTo { context: BrowseContext; pinned: PinnedTarget | null; facet: Facet }

/** The order the tabs are drawn in, which is also the order facetsFor returns. */
const ALL_FACETS: Facet[] = ['browse', 'activity', 'settings'];
/** The fleet target's tabs, in order: Members is its Browse. */
export const FLEET_FACETS: Facet[] = ['members', 'activity', 'settings'];

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
 * - A pinned target (archived, a create not yet registered, a registered repo
 *   with no store, a lens with a mount that has no store): Settings only. There is no store to browse and nobody
 *   bound to it.
 * - A draft: nothing. It is a form, not a thing with pages.
 *
 * A listed repo with no store as the CONTEXT (which only a stale persisted
 * value could produce; the rail pins one instead) gets Settings alone, the safe
 * answer. A repo the list does not carry yet gets all three (the list lags a
 * rename by one refresh) — unless a create is still working on it, which is
 * checked first and gives Settings only.
 */
export function facetsFor(target: Target, repos: RepoInfo[], lenses: Lens[], creates: RepoCreateStatus[]): Facet[] {
  if (target.kind === 'draft') return [];
  if (target.kind === 'fleet') return FLEET_FACETS;
  if (target.kind === 'archived' || target.kind === 'pending' || target.kind === 'unavailable'
    || target.kind === 'brokenLens') return ['settings'];
  if (target.kind === 'repo') {
    // A create still working on it comes FIRST: the create's repo may not be
    // in the listing yet, and it is Settings-only either way (the create block
    // is where it is reported; Browse has no create banner).
    if (activeCreateByRepo(creates).has(target.repo)) return ['settings'];
    const repo = repos.find(r => r.name === target.repo);
    // Not listed (yet): the list can lag a rename by a refresh, and the
    // context's own reads will say if it is really gone.
    if (!repo) return ALL_FACETS;
    if (!repoAvailable(repo)) return ['settings'];
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
  /** A settings-only target shown over the context. Never persisted: a reload
   *  lands on the remembered context. */
  pinned: PinnedTarget | null;
  /** The draft in the rail, if any. It stays mounted while another target is
   *  shown, so a half-filled form and a probe in flight survive a detour. */
  draft: DraftKind | null;
  /** The draft is the displayed target. */
  draftShown: boolean;
  /** Where Cancel on the draft goes back to: what was shown before it. */
  returnTo: ReturnTo | null;
  /** The server page on screen, or null. While set it is what the body shows,
   *  OVER the displayed target, which is left exactly as it was: closing it
   *  (the gear again) is the way back to that target and facet. Never
   *  persisted: a reload lands on the remembered target. */
  server: ServerPage | null;
}

export type ViewAction =
  | { type: 'TOGGLE_RAIL' }
  | { type: 'SET_RAIL'; open: boolean }
  /** A tab click (or a route that means one: Reconnect, entering a branch). */
  | { type: 'SET_FACET'; facet: Facet; focus?: string; keepServer?: boolean }
  /** A normal target was picked; `available` is facetsFor(that target). */
  | { type: 'SELECT_CONTEXT_TARGET'; available: Facet[]; keepServer?: boolean }
  | { type: 'SET_FOCUS'; focus: string }
  | { type: 'CLEAR_FOCUS' }
  | { type: 'SET_LOCKED'; locked: boolean }
  /** A pinned target was picked. Its facets are `available` (facetsFor);
   *  absent, Settings only, which is every pinned target but the fleet. */
  | { type: 'SELECT_PINNED'; target: PinnedTarget; available?: Facet[] }
  /** A rail `+`: insert the draft (or show the one already there). `from` is
   *  the browse context, recorded for Cancel. */
  | { type: 'OPEN_DRAFT'; kind: DraftKind; from: BrowseContext }
  /** The draft row was picked again. */
  | { type: 'SHOW_DRAFT'; from: BrowseContext }
  /** Cancel: the draft goes and the target and facet before it come back. The
   *  context half is App's to restore (it is AppState). */
  | { type: 'CANCEL_DRAFT' }
  /** The draft's target now exists: the draft row gives way to the real row.
   *  If the draft was on screen the reader lands on `facet` of the new target
   *  (Settings for a repo, which is still being created; Browse for a lens);
   *  if it was hidden, they are left where they are. `kind` is the draft that
   *  registered: a late result for a draft whose slot the other kind has
   *  since taken retires nothing. */
  | { type: 'DRAFT_REGISTERED'; kind: DraftKind; facet: Facet }
  /** The gear: open the server pages on `page`, or close them. */
  | { type: 'TOGGLE_SERVER' }
  /** A server tab click. */
  | { type: 'SET_SERVER_PAGE'; page: ServerPage };

/** initView reads the persisted parts of the view. Used as useReducer's init. */
export function initView(): ViewState {
  return {
    railOpen: loadRailOpen(), facet: loadLastFacet(), lastFacet: null, focus: null, locked: false,
    pinned: null, draft: null, draftShown: false, returnTo: null, server: null,
  };
}

/** The facet rule, shared by every way of selecting a target: restore the
 *  remembered facet if the target has it, else keep the facet if it has it,
 *  else Settings — which every target with facets has — remembering what the
 *  reader was on so the next normal target can restore it. */
function applyFacetRule(s: ViewState, available: Facet[]): ViewState {
  if (s.lastFacet && available.includes(s.lastFacet)) {
    return { ...s, facet: s.lastFacet, lastFacet: null, focus: null };
  }
  if (available.includes(s.facet)) return s.focus === null ? s : { ...s, focus: null };
  return { ...s, lastFacet: s.lastFacet ?? s.facet, facet: landingFacet(s.facet, available), focus: null };
}

/** Where a target lands when it lacks the current facet: Settings, which
 *  every target with facets has, except on two journeys that would make it a
 *  surprise. Into the fleet, its first tab (Members), as the prototype lands;
 *  out of the fleet's Members (a page no other target has), the target's own
 *  first tab, Browse when it has one. */
function landingFacet(current: Facet, available: Facet[]): Facet {
  if (available[0] === 'members') return 'members';
  if (current === 'members' && available.includes('browse')) return 'browse';
  return 'settings';
}

/** What Cancel on the draft returns to: the target on screen before it. Left
 *  as it was when the draft is already the one on screen. */
function returnToFrom(s: ViewState, from: BrowseContext): ReturnTo | null {
  return s.draftShown ? s.returnTo : { context: from, pinned: s.pinned, facet: s.facet };
}

/** Whether the target rail is on screen: the reader's preference, except that
 *  it is never shown over the server pages (which are not about a target), and
 *  is forced open while there are no repositories (`noRepos`, not persisted).
 *  The one place that rule lives: the bar, the shell and the keys read it. */
export function railShown(s: ViewState, noRepos = false): boolean {
  return s.server === null && (s.railOpen || noRepos);
}

export function viewReducer(s: ViewState, a: ViewAction): ViewState {
  switch (a.type) {
    // railOpen is the reader's PREFERENCE, and the server pages do not touch
    // it: they hide the rail (railShown) and refuse to change it, so it is
    // exactly as they found it when they close.
    case 'TOGGLE_RAIL':
      return s.server !== null ? s : { ...s, railOpen: !s.railOpen };
    case 'SET_RAIL':
      return s.server !== null || s.railOpen === a.open ? s : { ...s, railOpen: a.open };
    case 'SET_FACET':
      // A deliberate choice of facet supersedes the remembered one: the next
      // target keeps what the reader just picked, not what they had before.
      // It is also a way back from the server pages (the banner's Reconnect):
      // a facet is a page of a target, so the target comes back on it.
      // `keepServer` is for the app's own corrections (a pin that vanished or
      // healed under the server pages): they fix the target underneath, and
      // nobody is taken off the page they chose.
      return { ...s, facet: a.facet, lastFacet: null, focus: a.focus ?? null, server: a.keepServer ? s.server : null };
    case 'SELECT_CONTEXT_TARGET': {
      // A normal target IS the context: whatever was pinned over it goes, and
      // a shown draft is hidden (kept, so its row can be picked again).
      // Picking any row is also the way off the server pages (but not an
      // automatic unpin: `keepServer`).
      const server = a.keepServer ? s.server : null;
      const base = s.pinned === null && !s.draftShown && s.server === server ? s : { ...s, pinned: null, draftShown: false, server };
      return applyFacetRule(base, a.available);
    }
    case 'SELECT_PINNED':
      return applyFacetRule({ ...s, pinned: a.target, draftShown: false, server: null }, a.available ?? ['settings']);
    case 'OPEN_DRAFT':
      if (s.draft === a.kind && s.draftShown) return s.server === null ? s : { ...s, server: null };
      return { ...s, draft: a.kind, draftShown: true, returnTo: returnToFrom(s, a.from), server: null };
    case 'SHOW_DRAFT':
      if (!s.draft) return s;
      if (s.draftShown) return s.server === null ? s : { ...s, server: null };
      return { ...s, draftShown: true, returnTo: returnToFrom(s, a.from), server: null };
    case 'CANCEL_DRAFT': {
      if (!s.draft) return s;
      const r = s.returnTo;
      return {
        ...s, draft: null, draftShown: false, returnTo: null,
        pinned: r ? r.pinned : s.pinned, facet: r ? r.facet : s.facet, focus: null,
      };
    }
    case 'DRAFT_REGISTERED': {
      // The repo wizard's create keeps polling after its draft is replaced by
      // the lens draft (one slot), so its result can arrive for a draft that
      // is no longer there. It must not clear the draft that is.
      if (s.draft !== a.kind) return s;
      const cleared = { ...s, draft: null, draftShown: false, returnTo: null };
      // Registered while the reader was elsewhere: the draft row gives way to
      // the real one, and nobody is yanked off the page they chose.
      if (!s.draftShown) return cleared;
      const landed = { ...cleared, pinned: null, focus: null };
      if (a.facet !== 'settings') return { ...landed, facet: a.facet, lastFacet: null };
      // Settings is where a repository being created lands (E3). The facet the
      // reader had is remembered, so the next normal target restores it; the
      // job reaching done changes nothing here.
      if (s.facet === 'settings') return landed;
      return { ...landed, facet: 'settings', lastFacet: s.lastFacet ?? s.facet };
    }
    case 'TOGGLE_SERVER':
      // Opening lands on Logs, the first page. Closing changes nothing else:
      // the target and its facet were never left. (App offers no gear when
      // serverPagesFor gives no page.)
      return { ...s, server: s.server === null ? 'logs' : null };
    case 'SET_SERVER_PAGE':
      return s.server === a.page ? s : { ...s, server: a.page };
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

/**
 * serverPagesFor: the server tabs, in order: Logs, then Authorizations when
 * there is an OAuth issuer (useOAuthAvailability). The fleet is not here: it
 * is a target in the rail. Both are OMITTED on a read-only server, whose
 * endpoints answer 403 for both, so a tab would be a control that cannot
 * work; with no page at all there is no gear.
 */
export function serverPagesFor(serverReadOnly: boolean, oauthAvailable: boolean): ServerPage[] {
  if (serverReadOnly) return [];
  return oauthAvailable ? ['logs', 'auth'] : ['logs'];
}

/** draftCancellable: whether the draft offers Cancel. A repository draft with
 *  no repositories is the only thing there is to show, so Cancel would be a
 *  button that visibly does nothing (the `repos.length === 0` rule). */
export function draftCancellable(kind: DraftKind, repos: RepoInfo[]): boolean {
  return kind !== 'repo' || repos.length > 0;
}

/**
 * displayedTarget: what the body shows, in precedence order — a shown draft,
 * then a pinned target, then the browse context. With no repositories there is
 * no context to show, so the repository draft is the fallback (and the rail's
 * only Repositories row).
 */
export function displayedTarget(view: ViewState, context: BrowseContext, noRepos: boolean): Target {
  if (view.draftShown && view.draft) return { kind: 'draft', draft: view.draft };
  if (view.pinned) return view.pinned;
  if (noRepos) return { kind: 'draft', draft: 'repo' };
  return context;
}

/** What the top bar's label says about the displayed target: its kind (the
 *  icon and colour), its name, and for a target with only Settings the state
 *  chip that explains why. A draft is labelled by what it will create. */
export interface TargetLabel {
  kind: 'repo' | 'lens' | 'fleet';
  name: string;
  chip?: { text: string; tone: 'archived' | 'bad' | 'creating' | 'fleet' | 'neutral' | 'pending' };
  draft?: boolean;
}

/** fleetChip is the fleet target's chip, from GET /fleet as the server says
 *  it: the own record's state on main when it is pending or revoked (the two
 *  that need saying), else the role. */
export function fleetChip(fleet: Pick<FleetStatus, 'master' | 'record_state'> | null): TargetLabel['chip'] {
  if (!fleet) return undefined;
  if (fleet.record_state === 'pending') return { text: 'record pending', tone: 'pending' };
  if (fleet.record_state === 'revoked') return { text: 'revoked', tone: 'bad' };
  return fleet.master ? { text: 'master', tone: 'fleet' } : { text: 'member', tone: 'neutral' };
}

/** What a draft is called in the rail, the crumb and its index heading. */
export const DRAFT_NAME: Record<DraftKind, string> = { repo: 'New repository', lens: 'New lens', fleet: 'New fleet' };

/** The chip word for a repo with no store: "store missing" for the common
 *  case, the state's own word otherwise (RepoStateChip's wording). */
function unavailableWord(state: string | undefined): string {
  return state === 'missing' ? 'store missing' : (state || 'unavailable');
}

export function targetLabelFor(
  target: Target, repos: RepoInfo[], archived: ArchivedRepo[], creates: RepoCreateStatus[],
  fleet: FleetStatus | null = null,
): TargetLabel {
  switch (target.kind) {
    case 'draft':
      return { kind: target.draft, name: DRAFT_NAME[target.draft], draft: true };
    case 'fleet':
      return { kind: 'fleet', name: fleet?.fleet_repo || 'fleet', chip: fleetChip(fleet) };
    case 'archived':
      return { kind: 'repo', name: archived.find(a => a.id === target.id)?.name ?? '', chip: { text: 'archived', tone: 'archived' } };
    case 'unavailable':
      return { kind: 'repo', name: target.repo, chip: { text: unavailableWord(repos.find(r => r.name === target.repo)?.state), tone: 'bad' } };
    case 'brokenLens':
      return { kind: 'lens', name: target.name, chip: { text: 'mount missing', tone: 'bad' } };
    case 'pending': {
      const job = creates.find(c => c.create_id === target.createId);
      const flag = job ? createFlag(job.state) : null;
      return {
        kind: 'repo', name: job?.name ?? '',
        ...(flag ? { chip: { text: flag, tone: flag === 'failed' ? 'bad' as const : 'creating' as const } } : {}),
      };
    }
    case 'repo': {
      const job = activeCreateByRepo(creates).get(target.repo);
      const flag = job ? createFlag(job.state) : null;
      return { kind: 'repo', name: target.repo, ...(flag ? { chip: { text: flag, tone: 'creating' as const } } : {}) };
    }
    case 'lens':
      return { kind: 'lens', name: target.name };
  }
}

/** useViewState is App's view reducer plus its persistence: knomit.rail and
 *  knomit.facet are written whenever they change. Nothing else in the view is
 *  remembered across a reload. */
export function useViewState(): [ViewState, Dispatch<ViewAction>] {
  const [view, dispatch] = useReducer(viewReducer, undefined, initView);
  useEffect(() => { saveRailOpen(view.railOpen); }, [view.railOpen]);
  // Members is the fleet's, and the fleet is never remembered across a
  // reload (a pinned target), so it is not stored either.
  useEffect(() => { if (view.facet !== 'members') saveLastFacet(view.facet); }, [view.facet]);
  return [view, dispatch];
}
