import type { Lens, LensSource } from './api';
import { displayLensPath } from './utils';

export type View = 'library';

export type LibrarySort = 'path' | 'recent' | 'relevance';

// BrowseContext is the surface the app is currently browsing: a whole repo, or
// a lens's read union. It is the single source of truth for "what am I looking
// at" — SET_REPO is a thin wrapper that dispatches SET_CONTEXT {kind:'repo'}.
export type BrowseContext =
  | { kind: 'repo'; repo: string }
  | { kind: 'lens'; name: string };

export interface FilterChip {
  // 'repo' is a lens-only facet (narrows the union fan-out); it never appears in
  // a repo context. See parseFilterQuery(opts.allowRepo) and FilterBar.
  category: 'domain' | 'entity' | 'type' | 'kind' | 'origin' | 'ep' | 'path' | 'repo';
  value: string;
}

export type AsOf =
  | { mode: 'live' }
  | { mode: 'history'; commit: string }
  | { mode: 'diff'; from: string; to: string };

interface NavEntry {
  repo: string;
  branch: string;
  view: View;
  filters: FilterChip[];
  freeText: string;
  factPath: string | null;
  asOf: AsOf;
  // The sort axis in force when this entry was left. Restored on NAV_BACK
  // because Path and Recent are different WAYS OF LOOKING, not a preference:
  // backing out of a chrono-sorted list into an ontology-sorted one silently
  // rearranges the rows under the cursor. SET_SORT itself does not push — a
  // sort change is a refinement of the current view, not a move.
  sort: LibrarySort;
}

export interface AppState {
  repo: string;
  // context is the browse surface (repo | lens). In a repo context, repo/branch
  // are authoritative for both reads and writes. In a lens context, reads come
  // via the lens endpoints (Task 14); repo/branch are kept valid pointing at the
  // lens's write mount + its agent branch so write routing never breaks.
  context: BrowseContext;
  lens: Lens | null;            // resolved lens doc; null in repo context
  lensSources: string[] | null; // sources-dropdown selection; null = all mounts
  // factSource is the source mount of the currently open lens fact — set by the
  // fact-open path when a lens fact loads (Task 14/16), and cleared on context
  // switch. It is the temporal/write anchor for the open fact in a lens context.
  factSource: LensSource | null;
  view: View;
  factPath: string | null;       // right panel: fact to display (all modes)
  asOf: AsOf;                    // global "as of when" anchor (live | history | diff)
  filters: FilterChip[];
  freeText: string;              // unprefixed search text
  tasks: Record<string, { status: 'idle' | 'running' | 'done' | 'error'; message: string }>;
  headCommit: string;
  branch: string;
  embeddingsEnabled: boolean;
  ontologyRoot: string;
  indexState: string;  // "ready" | "indexing" | "error"
  indexDone: number;
  indexTotal: number;
  indexPercent: number;  // 0–100; 100 when ready
  navStack: NavEntry[];
  // The remote's two halves fail INDEPENDENTLY — a fetch can recover while a
  // push is still rejected — and each maps 1:1 onto the column Sync/Push write
  // (remotes.last_status / last_push_status). Tracking them apart is what stops
  // a sync_ok from clearing a banner a still-broken push raised. Read them
  // through remoteErrorText, never directly.
  remoteSyncError: string;
  remotePushError: string;
  rightPanelFocused: boolean;
  librarySort: LibrarySort;
  notice: string;
  searching: boolean;            // a relevance (free-text) search request is in flight
  serverReadOnly: boolean;       // instance-level read-only (demo mode)
  // factTitles caches the human title of every fact the RightPanel has loaded,
  // keyed by factTitleKey(path, commit). The breadcrumb reads from it so it can
  // label a crumb with the title we ALREADY read when navigating there — rather
  // than re-fetching, which fails for a retracted fact (the live single-fact
  // endpoint 404s a tombstone). Session-only: the trail is session-only too, so
  // every crumb was visited and is therefore cached.
  factTitles: Record<string, string>;
}

// factTitleKey is the shared cache key for a fact's title: its path plus the
// commit it was read at (undefined = live/HEAD). The breadcrumb's crumb key and
// the RightPanel's cache write MUST agree, so both go through this.
export function factTitleKey(path: string, commit?: string): string {
  return `${path}@${commit ?? 'HEAD'}`;
}

export type Action =
  | { type: 'NAVIGATE'; path: string }
  | { type: 'GO_UP' }
  | { type: 'ADD_FILTER'; chip: FilterChip }
  | { type: 'REMOVE_FILTER'; index: number }
  | { type: 'SET_FREE_TEXT'; text: string }
  | { type: 'CLEAR_FILTERS' }
  | { type: 'NAV_BACK' }
  | { type: 'SET_TASK'; op: string; status: 'idle' | 'running' | 'done' | 'error'; message: string }
  | { type: 'CLEAR_TASK'; op: string }
  | { type: 'SET_STATUS'; head: string; branch: string; embeddingsEnabled: boolean; ontologyRoot: string; indexState?: string; indexDone?: number; indexTotal?: number; indexPercent?: number }
  | { type: 'SET_HEAD'; head: string }
  | { type: 'SET_REPO'; repo: string }
  | { type: 'SET_CONTEXT'; context: BrowseContext }
  | { type: 'CACHE_FACT_TITLE'; key: string; title: string }
  | { type: 'SET_LENS'; lens: Lens }
  | { type: 'SET_LENS_SOURCES'; repos: string[] | null }
  | { type: 'SET_FACT_SOURCE'; source: LensSource | null }
  | { type: 'SET_REMOTE_ERROR'; side: 'sync' | 'push'; error: string }
  | { type: 'CLEAR_REMOTE_ERRORS' }
  | { type: 'FOCUS_RIGHT_PANEL' }
  | { type: 'BLUR_RIGHT_PANEL' }
  | { type: 'SET_AS_OF'; asOf: AsOf }
  | { type: 'APPLY_NAV'; view: View; factPath: string | null; asOf: AsOf; filters?: FilterChip[]; freeText?: string; hop?: boolean }
  | { type: 'AMEND_NAV'; factPath: string | null; asOf?: AsOf }
  | { type: 'SET_LIBRARY_SORT'; sort: LibrarySort }
  | { type: 'SET_NOTICE'; text: string }
  | { type: 'CLEAR_NOTICE' }
  | { type: 'SET_SEARCHING'; value: boolean }
  | { type: 'SET_SERVER_READONLY'; value: boolean };

export const init: AppState = {
  // No repo is selected until the server's repo list loads — the UI must never
  // assume a repo name exists (any repo, including the default, can be renamed
  // or deleted server-side). App picks the repo from /api/v1/repos on mount.
  repo: '',
  context: { kind: 'repo', repo: '' },
  lens: null,
  lensSources: null,
  factSource: null,
  view: 'library',
  factPath: null,
  asOf: { mode: 'live' },
  filters: [],
  freeText: '',
  tasks: { sync: { status: 'idle', message: '' }, synth: { status: 'idle', message: '' } },
  headCommit: '',
  branch: '',
  embeddingsEnabled: false,
  ontologyRoot: 'kb',
  indexState: 'ready',
  indexDone: 0,
  indexTotal: 0,
  indexPercent: 100,
  navStack: [],
  remoteSyncError: '',
  remotePushError: '',
  rightPanelFocused: false,
  // Ontology browsing, not chrono: the tree is how the corpus is organised, so
  // arriving at a folder listing beats arriving at a flat by-date feed.
  librarySort: 'path',
  notice: '',
  searching: false,
  serverReadOnly: false,
  factTitles: {},
};

function pushNav(s: AppState): NavEntry[] {
  const entry: NavEntry = {
    repo: s.repo,
    branch: s.branch,
    view: s.view,
    filters: [...s.filters],
    freeText: s.freeText,
    factPath: s.factPath,
    asOf: s.asOf,
    sort: s.librarySort,
  };
  const stack = [...s.navStack, entry];
  if (stack.length > 20) stack.shift();
  return stack;
}

// canGoBack reports whether NAV_BACK has anywhere to go. There is no forward:
// NAV_BACK pops, so the entries ahead of the cursor do not exist to return to.
// A back-only history is the whole model here — see the Library header.
export function canGoBack(state: AppState): boolean {
  return state.navStack.length > 0;
}

export function currentPath(state: AppState): string {
  const pathChip = state.filters.find(f => f.category === 'path');
  return pathChip?.value || state.ontologyRoot || 'kb';
}

function replacePathChip(filters: FilterChip[], value: string): FilterChip[] {
  return [...filters.filter(f => f.category !== 'path'), { category: 'path', value }];
}


function applyAction(s: AppState, a: Action): AppState {
  switch (a.type) {
    case 'NAVIGATE':
      return {
        ...s,
        filters: replacePathChip(s.filters, a.path),
        factPath: null,
        navStack: pushNav(s),
        rightPanelFocused: false,
      };
    case 'GO_UP': {
      const path = currentPath(s);
      const parts = path.split('/');
      if (parts.length <= 1) return s;
      const parent = parts.slice(0, -1).join('/');
      return {
        ...s,
        filters: replacePathChip(s.filters, parent),
        factPath: null,
        navStack: pushNav(s),
        rightPanelFocused: false,
      };
    }
    case 'ADD_FILTER': {
      const isPath = a.chip.category === 'path';
      const filters = isPath
        ? replacePathChip(s.filters, a.chip.value)
        : [...s.filters, a.chip];
      // Path-changing filters are navigations; clear the open fact so the
      // right panel returns to the stats view for the new path. Non-path
      // filters are refinements that should preserve the current selection.
      return { ...s, filters, factPath: isPath ? null : s.factPath, navStack: pushNav(s) };
    }
    case 'REMOVE_FILTER': {
      const filters = s.filters.filter((_, i) => i !== a.index);
      return { ...s, filters, factPath: null, navStack: pushNav(s) };
    }
    case 'SET_FREE_TEXT': {
      // When clearing the search box leaves no active non-path filters, the
      // user has exited search mode — drop the (typically auto-selected)
      // factPath so the right panel returns to root stats instead of stranding
      // the previous result.
      const exitingSearch = a.text === '' && !s.filters.some(f => f.category !== 'path');
      return { ...s, freeText: a.text, factPath: exitingSearch ? null : s.factPath };
    }
    case 'CLEAR_FILTERS':
      return { ...s, filters: [], freeText: '', factPath: null, navStack: pushNav(s) };
    case 'NAV_BACK': {
      if (s.navStack.length === 0) return s;
      const prev = s.navStack[s.navStack.length - 1];
      // If repo changed, treat as full reset
      if (prev.repo !== s.repo) {
        return {
          ...s,
          repo: prev.repo,
          view: 'library',
          factPath: null,
          asOf: { mode: 'live' },
          filters: [],
          freeText: '',
          headCommit: '',
          branch: '',
          librarySort: prev.sort,
          navStack: s.navStack.slice(0, -1),
        };
      }
      return {
        ...s,
        view: prev.view,
        factPath: prev.factPath,
        asOf: prev.asOf,
        filters: prev.filters,
        freeText: prev.freeText,
        librarySort: prev.sort,
        navStack: s.navStack.slice(0, -1),
        rightPanelFocused: false,
      };
    }
    case 'SET_TASK': {
      const cur = s.tasks[a.op];
      if (cur && cur.status === a.status && cur.message === a.message) return s;
      return { ...s, tasks: { ...s.tasks, [a.op]: { status: a.status, message: a.message } } };
    }
    case 'CLEAR_TASK': {
      // Retires a FINISHED task back to idle. Only the terminal states are
      // retired: a running task owns the footer for as long as it runs, and
      // dropping one mid-flight would hide live progress. Without this nothing
      // ever left the footer, so the last "done" of the session sat there
      // reading like something still happening.
      const cur = s.tasks[a.op];
      if (!cur || cur.status === 'idle' || cur.status === 'running') return s;
      return { ...s, tasks: { ...s.tasks, [a.op]: { status: 'idle', message: '' } } };
    }
    case 'SET_STATUS':
      return {
        ...s,
        headCommit: a.head,
        branch: a.branch,
        embeddingsEnabled: a.embeddingsEnabled,
        ontologyRoot: a.ontologyRoot || s.ontologyRoot,
        indexState: a.indexState ?? s.indexState,
        indexDone: a.indexDone ?? s.indexDone,
        indexTotal: a.indexTotal ?? s.indexTotal,
        indexPercent: a.indexPercent ?? s.indexPercent,
      };
    case 'SET_HEAD':
      if (s.headCommit === a.head) return s;
      return { ...s, headCommit: a.head };
    case 'SET_REPO':
      // Thin wrapper: switching to a repo is just entering a {kind:'repo'}
      // context. Reducing through SET_CONTEXT keeps a single reset path so repo
      // and lens switches can never drift apart.
      return reducer(s, { type: 'SET_CONTEXT', context: { kind: 'repo', repo: a.repo } });
    case 'SET_CONTEXT': {
      // Entering any browse surface resets the navigation state exactly as a
      // repo switch always did: asOf → live, open fact closed, filters/freeText
      // cleared, nav trail dropped, remote error cleared, right panel blurred.
      // factSource is dropped too — the open fact (and its source mount) is gone.
      const base: AppState = {
        ...s,
        context: a.context,
        view: 'library',
        factPath: null,
        asOf: { mode: 'live' },
        filters: [],
        freeText: '',
        navStack: [],
        remoteSyncError: '',
        remotePushError: '',
        rightPanelFocused: false,
        factSource: null,
      };
      if (a.context.kind === 'repo') {
        // Repo context: repo/branch are authoritative. Clearing branch/headCommit
        // re-triggers the status bootstrap. lens state is not applicable.
        return { ...base, repo: a.context.repo, headCommit: '', branch: '', lens: null, lensSources: null };
      }
      // Lens context: the write mount isn't known until SET_LENS resolves the
      // lens doc, so repo/branch stay at their previous (still valid) values
      // until then. lens/lensSources reset; App re-fetches the lens.
      return { ...base, lens: null, lensSources: null };
    }
    case 'SET_LENS':
      // Guard against a stale, out-of-order resolution. resolveLens is
      // fire-and-forget: a slow getLens(A) can resolve after the user already
      // switched to lens B or back to a repo context. Applying it would repoint
      // repo at A's write mount and set lens=A in the wrong context (violating
      // "lens is null in a repo context"). Only accept a lens doc that still
      // matches the active lens context.
      if (s.context.kind !== 'lens' || s.context.name !== a.lens.name) return s;
      // The lens's write mount becomes the app's write/status target so
      // state.repo/state.branch stay valid in a lens context. When the write
      // repo actually changes, clear branch/headCommit to re-run the status
      // bootstrap (which resolves the write repo's agent branch).
      return {
        ...s,
        lens: a.lens,
        repo: a.lens.write,
        branch: s.repo === a.lens.write ? s.branch : '',
        headCommit: s.repo === a.lens.write ? s.headCommit : '',
      };
    case 'SET_LENS_SOURCES':
      return { ...s, lensSources: a.repos };
    case 'SET_FACT_SOURCE':
      return { ...s, factSource: a.source };
    case 'SET_REMOTE_ERROR':
      // Scoped to ONE side. sync_ok says the fetch half is healthy and says
      // NOTHING about the push half, so clearing both here is how a sync_ok
      // used to wipe a banner an expired push token had raised — leaving the
      // next failing push tick to put it straight back, once per reconcile
      // interval, for as long as the token stayed broken.
      //
      // Identity-stable when nothing changed. This action is dispatched by
      // POLLS (the persisted-status re-read on repo switch, reconnect, and
      // repo-manager close), not just by remote events, and re-confirming the
      // same value must not mint a new AppState and re-render every panel.
      if (a.side === 'push') {
        if (s.remotePushError === a.error) return s;
        return { ...s, remotePushError: a.error };
      }
      if (s.remoteSyncError === a.error) return s;
      return { ...s, remoteSyncError: a.error };
    case 'CLEAR_REMOTE_ERRORS':
      // The user dismissing the banner acknowledges BOTH sides — it is one
      // banner, and there is no way to acknowledge half of it. Nothing is lost:
      // a side that is still broken re-raises on its next failing tick.
      if (!s.remoteSyncError && !s.remotePushError) return s;
      return { ...s, remoteSyncError: '', remotePushError: '' };
    case 'FOCUS_RIGHT_PANEL':
      return { ...s, rightPanelFocused: true };
    case 'BLUR_RIGHT_PANEL':
      return { ...s, rightPanelFocused: false };
    case 'SET_AS_OF':
      return { ...s, asOf: a.asOf };
    case 'SET_LIBRARY_SORT':
      // Switching sort clears the selected fact so the right panel doesn't
      // strand a previous selection in the new view. Recent/Relevance modes
      // auto-select their first row after the fetch settles; Path mode
      // starts un-selected so the user picks deliberately from the tree.
      return { ...s, librarySort: a.sort, factPath: null };
    case 'SET_NOTICE':
      return { ...s, notice: a.text };
    case 'CLEAR_NOTICE':
      return s.notice === '' ? s : { ...s, notice: '' };
    case 'SET_SEARCHING':
      return s.searching === a.value ? s : { ...s, searching: a.value };
    case 'SET_SERVER_READONLY':
      return { ...s, serverReadOnly: a.value };
    case 'APPLY_NAV': {
      // Cycle-collapse: a subject hop (hop:true) to a fact already in the trail
      // unwinds to the existing crumb instead of pushing a duplicate. This is
      // the single chokepoint for ALL link-following navigation (edge refs,
      // in-body refs, timeline files-affected), so cycles can't accumulate no
      // matter which surface the hop came from. Deliberate navigations
      // (library selection, return-to-live) omit hop and always push.
      if (a.hop && a.factPath != null) {
        const plan = planTrailHop(selectTrail(s), a.factPath);
        if (plan.kind === 'unwind') {
          let next = s;
          for (let k = 0; k < plan.steps; k++) next = reducer(next, { type: 'NAV_BACK' });
          return next;
        }
      }
      return {
        ...s,
        view: a.view,
        factPath: a.factPath,
        asOf: a.asOf,
        filters: a.filters !== undefined ? a.filters : s.filters,
        freeText: a.freeText !== undefined ? a.freeText : s.freeText,
        navStack: pushNav(s),
        rightPanelFocused: false,
      };
    }
    case 'AMEND_NAV': {
      // In-place update — no navStack push. Used by auto-select behaviors so that
      // a single user action (e.g. view-button click) creates exactly one navStack entry.
      if (s.factPath === a.factPath && (a.asOf === undefined || JSON.stringify(s.asOf) === JSON.stringify(a.asOf))) return s;
      return {
        ...s,
        factPath: a.factPath,
        ...(a.asOf !== undefined ? { asOf: a.asOf } : {}),
      };
    }
    case 'CACHE_FACT_TITLE': {
      // No-op when nothing changes so a re-fire never triggers a needless render.
      if (!a.title || s.factTitles[a.key] === a.title) return s;
      return { ...s, factTitles: { ...s.factTitles, [a.key]: a.title } };
    }
    default:
      return s;
  }
}

// reducer wraps applyAction with a cross-cutting invariant: factSource is
// meaningful only while a fact is open. Whenever a fact closes (factPath -> null)
// — via APPLY_NAV/AMEND_NAV to null, NAVIGATE, GO_UP, filter changes, sort
// switch, context switch, etc. — drop the source mount so no direct reader can
// observe stale data. Centralizing this here means new fact-close paths inherit
// the guarantee for free. Referential identity is preserved for the common case
// (already-null factSource), so no-op arms that `return s` stay ===.
export function reducer(s: AppState, a: Action): AppState {
  const next = applyAction(s, a);
  if (next.factPath === null && next.factSource !== null) {
    return { ...next, factSource: null };
  }
  return next;
}

export function selectAnchorCommit(s: AppState): string | null {
  switch (s.asOf.mode) {
    case 'live':     return null;
    case 'history': return s.asOf.commit;
    case 'diff':     return s.asOf.to;
  }
}

export function isLive(s: AppState): boolean {
  return s.asOf.mode === 'live';
}

// isLensContext is true when the app is browsing a lens (rather than a repo).
export function isLensContext(s: AppState): boolean {
  return s.context.kind === 'lens';
}

// lensResolutionPending is true when the context names a lens that state.lens
// does not yet reflect — the signal for App's resolution effect. A lens context
// can be entered from several surfaces (TopBar switcher, manager Browse,
// bootstrap restore); they all just dispatch SET_CONTEXT, and App resolves
// whenever this returns true. Same-name re-resolution after an edit is
// deliberately NOT covered (state.lens.name already matches) — that path goes
// through refreshContextAfterChange.
export function lensResolutionPending(s: AppState): boolean {
  return s.context.kind === 'lens' && s.lens?.name !== s.context.name;
}

// openFactSource is THE temporal/write anchor for the current view:
//   - repo context      → {state.repo, state.branch} (authoritative)
//   - lens context, fact open → the open fact's source mount (repo + the branch
//     it was read at), so writes/history for that fact route to where it lives.
//   - lens context, no fact   → the lens's write mount, {lens.write, ''}.
// A branch of '' is the "resolve the agent branch" sentinel: callers translate
// it (via api.getAgentBranch) before issuing a write. A closed fact (factPath
// null) is treated as "no fact open" so a stale factSource can never leak past
// a fact close — the fact-open path re-sets factSource for the next lens fact.
export function openFactSource(s: AppState): { repo: string; branch: string } {
  if (s.context.kind === 'lens') {
    if (s.factPath && s.factSource) {
      return { repo: s.factSource.repo, branch: s.factSource.branch };
    }
    if (s.lens) return { repo: s.lens.write, branch: '' };
  }
  return { repo: s.repo, branch: s.branch };
}

// factHistoryAnchor is the READ anchor for a fact's history/edges: the open
// fact's source mount (openFactSource) paired with the RELATIVE path (the
// kb://<id12>/ qualifier stripped via displayLensPath). Co-locating repo,
// branch, AND path in one helper keeps the three dimensions from drifting apart
// at the call sites (LeftPanel/App/RightPanel/useTimeTravel) — the drift that
// let a browse-surface commit leak against a read mount. `path` defaults to the
// open fact; pass the loaded fact's own canonical path where it differs. Repo
// context: {state.repo, state.branch, <bare path>} — byte-identical to the old
// inline pairing.
export function factHistoryAnchor(
  s: AppState,
  path: string | null = s.factPath,
): { repo: string; branch: string; path: string } {
  const { repo, branch } = openFactSource(s);
  return { repo, branch, path: displayLensPath(path ?? '') };
}

// edgeAnchorCommit is the commit useFactEdges anchors on. Time-travelling: the
// history/diff anchor (selectAnchorCommit) — a commit drawn from the OPEN FACT's
// own mount timeline, so it resolves against that mount. Live: the mount's live
// HEAD — state.headCommit in a repo context (the repo's own head), but '' (no
// commit → the non-anchored live-HEAD explain URL) in a lens context, where the
// open fact's mount head isn't tracked in state and state.headCommit is the
// WRITE repo's head (which doesn't exist in a read mount → an empty edge set in
// the default live view). Repo context is byte-identical (always state.headCommit).
export function edgeAnchorCommit(s: AppState): string {
  return selectAnchorCommit(s) ?? (isLensContext(s) ? '' : s.headCommit);
}

// remoteErrorText is the single line the banner shows for an unhealthy remote,
// and the condition everything else gates on: empty means healthy on BOTH sides.
// The two halves are tracked apart (see remoteSyncError / remotePushError) but
// only one message fits one banner, so the fetch side wins when both are set —
// a fetch that cannot reach origin is the more fundamental failure, and the push
// error is usually a consequence of it rather than separate news.
export function remoteErrorText(s: AppState): string {
  return s.remoteSyncError || s.remotePushError;
}

export function isReadOnly(s: AppState): boolean {
  return s.serverReadOnly || !isLive(s);
}

export const READ_ONLY_TITLE = 'Read-only — anchor is not live';

export interface TrailCrumb {
  factPath: string;
  asOf: AsOf;
}

// The current view is the last crumb. In a history excursion the trail also
// includes the prior subject hops back to the live root (the most recent
// fact-bearing entry that was live). Pure time-scrubs (SET_AS_OF, no navStack
// push) don't add crumbs — only subject hops (APPLY_NAV with a factPath) do.
export function selectTrail(s: AppState): TrailCrumb[] {
  const current: TrailCrumb = { factPath: s.factPath ?? '', asOf: s.asOf };
  if (isLive(s)) return [current];
  const prefix: TrailCrumb[] = [];
  for (let i = s.navStack.length - 1; i >= 0; i--) {
    const e = s.navStack[i];
    if (e.factPath == null) continue;
    prefix.unshift({ factPath: e.factPath, asOf: e.asOf });
    if (e.asOf.mode === 'live') break;
  }
  return [...prefix, current];
}

/**
 * Decide how a subject hop to `targetPath` should affect the trail.
 *
 * If the target fact already appears in the trail, the user is revisiting a
 * fact they came from (A → B → A …). Rather than push a duplicate crumb — which
 * grows a repeating cycle in the breadcrumb — unwind back to the existing crumb.
 * `steps` is how many NAV_BACK pops land on it (0 = already current, a no-op).
 * Otherwise push a fresh crumb. Matched on `factPath` (subject identity), so a
 * revisit at a different version still collapses to the crumb already there.
 */
export function planTrailHop(
  trail: TrailCrumb[],
  targetPath: string,
): { kind: 'unwind'; steps: number } | { kind: 'push' } {
  const depth = trail.length - 1;
  const i = trail.findIndex(c => c.factPath === targetPath);
  if (i >= 0) return { kind: 'unwind', steps: depth - i };
  return { kind: 'push' };
}
