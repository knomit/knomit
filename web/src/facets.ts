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
// targets and the draft. The server pages join it when they leave Manage.

import { useEffect, useReducer } from 'react';
import type { Dispatch } from 'react';
import { loadRailOpen, saveRailOpen, loadLastFacet, saveLastFacet } from './repoSelection';
import { repoAvailable, brokenLensMember } from './api';
import type { RepoInfo, Lens, RepoCreateStatus, ArchivedRepo } from './api';
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

/** A page of a target. Code joins this only if the terminal ships. */
export type Facet = 'browse' | 'activity' | 'settings';

/**
 * A target that can NOT be the browse context, because it has no store to read:
 * an archived repo, a create that is not registered yet, a registered repo whose
 * store did not open. It is pinned OVER the context, which stays where it was,
 * so leaving it lands back on the context with nothing to restore.
 */
export type PinnedTarget =
  | { kind: 'archived'; id: string }
  | { kind: 'pending'; createId: string }
  | { kind: 'unavailable'; repo: string };

export type DraftKind = 'repo' | 'lens';

/** Anything the rail can select: a normal target (the browse context), a
 *  pinned one, or a draft. */
export type Target = BrowseContext | PinnedTarget | { kind: 'draft'; draft: DraftKind };

/** Where Cancel on a draft goes back to. */
export interface ReturnTo { context: BrowseContext; pinned: PinnedTarget | null; facet: Facet }

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
 * - A pinned target (archived, a create not yet registered, a registered repo
 *   with no store): Settings only. There is no store to browse and nobody
 *   bound to it.
 * - A draft: nothing. It is a form, not a thing with pages.
 *
 * A listed repo with no store as the CONTEXT (which only a stale persisted
 * value could produce; the rail pins one instead) gets Settings alone, the safe
 * answer. A repo the list does not carry yet gets all three: the list lags a
 * create or a rename by one refresh.
 */
export function facetsFor(target: Target, repos: RepoInfo[], lenses: Lens[], creates: RepoCreateStatus[]): Facet[] {
  if (target.kind === 'draft') return [];
  if (target.kind === 'archived' || target.kind === 'pending' || target.kind === 'unavailable') return ['settings'];
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
  | { type: 'SET_LOCKED'; locked: boolean }
  /** A settings-only target was picked. Its only facet is Settings. */
  | { type: 'SELECT_PINNED'; target: PinnedTarget }
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
   *  if it was hidden, they are left where they are. */
  | { type: 'DRAFT_REGISTERED'; facet: Facet };

/** initView reads the persisted parts of the view. Used as useReducer's init. */
export function initView(): ViewState {
  return {
    railOpen: loadRailOpen(), facet: loadLastFacet(), lastFacet: null, focus: null, locked: false,
    pinned: null, draft: null, draftShown: false, returnTo: null,
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
  return { ...s, lastFacet: s.lastFacet ?? s.facet, facet: 'settings', focus: null };
}

/** What Cancel on the draft returns to: the target on screen before it. Left
 *  as it was when the draft is already the one on screen. */
function returnToFrom(s: ViewState, from: BrowseContext): ReturnTo | null {
  return s.draftShown ? s.returnTo : { context: from, pinned: s.pinned, facet: s.facet };
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
      // A normal target IS the context: whatever was pinned over it goes, and
      // a shown draft is hidden (kept, so its row can be picked again).
      const base = s.pinned === null && !s.draftShown ? s : { ...s, pinned: null, draftShown: false };
      return applyFacetRule(base, a.available);
    }
    case 'SELECT_PINNED':
      return applyFacetRule({ ...s, pinned: a.target, draftShown: false }, ['settings']);
    case 'OPEN_DRAFT':
      if (s.draft === a.kind && s.draftShown) return s;
      return { ...s, draft: a.kind, draftShown: true, returnTo: returnToFrom(s, a.from) };
    case 'SHOW_DRAFT':
      if (!s.draft || s.draftShown) return s;
      return { ...s, draftShown: true, returnTo: returnToFrom(s, a.from) };
    case 'CANCEL_DRAFT': {
      if (!s.draft) return s;
      const r = s.returnTo;
      return {
        ...s, draft: null, draftShown: false, returnTo: null,
        pinned: r ? r.pinned : s.pinned, facet: r ? r.facet : s.facet, focus: null,
      };
    }
    case 'DRAFT_REGISTERED': {
      if (!s.draft) return s;
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

/** draftCancellable: whether the draft offers Cancel. A repository draft with
 *  no repositories is the only thing there is to show, so Cancel would be a
 *  button that visibly does nothing (the `repos.length === 0` rule). */
export function draftCancellable(kind: DraftKind, repos: RepoInfo[]): boolean {
  return kind === 'lens' || repos.length > 0;
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
  kind: 'repo' | 'lens';
  name: string;
  chip?: { text: string; tone: 'archived' | 'bad' | 'creating' };
  draft?: boolean;
}

/** The chip word for a repo with no store: "store missing" for the common
 *  case, the state's own word otherwise (RepoStateChip's wording). */
function unavailableWord(state: string | undefined): string {
  return state === 'missing' ? 'store missing' : (state || 'unavailable');
}

export function targetLabelFor(
  target: Target, repos: RepoInfo[], archived: ArchivedRepo[], creates: RepoCreateStatus[],
): TargetLabel {
  switch (target.kind) {
    case 'draft':
      return { kind: target.draft, name: target.draft === 'repo' ? 'New repository' : 'New lens', draft: true };
    case 'archived':
      return { kind: 'repo', name: archived.find(a => a.id === target.id)?.name ?? '', chip: { text: 'archived', tone: 'archived' } };
    case 'unavailable':
      return { kind: 'repo', name: target.repo, chip: { text: unavailableWord(repos.find(r => r.name === target.repo)?.state), tone: 'bad' } };
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
  useEffect(() => { saveLastFacet(view.facet); }, [view.facet]);
  return [view, dispatch];
}
