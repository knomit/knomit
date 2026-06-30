export type View = 'library';

export type LibrarySort = 'path' | 'recent' | 'relevance';

export interface FilterChip {
  category: 'domain' | 'entity' | 'type' | 'kind' | 'origin' | 'ep' | 'path';
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
}

interface ConsoleEntry {
  id: number;
  time: number; // Date.now()
  level: 'info' | 'error';
  message: string;
}

export interface AppState {
  repo: string;
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
  consoleEntries: ConsoleEntry[];
  consoleOpen: boolean;
  consoleHeight: number;
  navStack: NavEntry[];
  remoteError: string;
  rightPanelFocused: boolean;
  librarySort: LibrarySort;
  notice: string;
  searching: boolean;            // a relevance (free-text) search request is in flight
  serverReadOnly: boolean;       // instance-level read-only (demo mode)
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
  | { type: 'SET_STATUS'; head: string; branch: string; embeddingsEnabled: boolean; ontologyRoot: string; indexState?: string; indexDone?: number; indexTotal?: number; indexPercent?: number }
  | { type: 'SET_HEAD'; head: string }
  | { type: 'CONSOLE_LOG'; level: 'info' | 'error'; message: string }
  | { type: 'CONSOLE_TOGGLE' }
  | { type: 'CONSOLE_SET_HEIGHT'; height: number }
  | { type: 'SET_REPO'; repo: string }
  | { type: 'SET_REMOTE_ERROR'; error: string }
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
  consoleEntries: [],
  consoleOpen: false,
  consoleHeight: 200,
  navStack: [],
  remoteError: '',
  rightPanelFocused: false,
  librarySort: 'recent',
  notice: '',
  searching: false,
  serverReadOnly: false,
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
  };
  const stack = [...s.navStack, entry];
  if (stack.length > 20) stack.shift();
  return stack;
}

export function currentPath(state: AppState): string {
  const pathChip = state.filters.find(f => f.category === 'path');
  return pathChip?.value || state.ontologyRoot || 'kb';
}

function replacePathChip(filters: FilterChip[], value: string): FilterChip[] {
  return [...filters.filter(f => f.category !== 'path'), { category: 'path', value }];
}


export function reducer(s: AppState, a: Action): AppState {
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
        navStack: s.navStack.slice(0, -1),
        rightPanelFocused: false,
      };
    }
    case 'SET_TASK': {
      const cur = s.tasks[a.op];
      if (cur && cur.status === a.status && cur.message === a.message) return s;
      return { ...s, tasks: { ...s.tasks, [a.op]: { status: a.status, message: a.message } } };
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
    case 'CONSOLE_LOG': {
      const entry: ConsoleEntry = { id: Date.now() + Math.random(), time: Date.now(), level: a.level, message: a.message };
      const entries = [...s.consoleEntries, entry];
      if (entries.length > 500) entries.splice(0, entries.length - 500);
      return { ...s, consoleEntries: entries };
    }
    case 'CONSOLE_TOGGLE':
      return { ...s, consoleOpen: !s.consoleOpen };
    case 'CONSOLE_SET_HEIGHT':
      return { ...s, consoleHeight: Math.max(80, Math.min(a.height, 600)) };
    case 'SET_REPO':
      return {
        ...s,
        repo: a.repo,
        view: 'library',
        factPath: null,
        asOf: { mode: 'live' },
        filters: [],
        freeText: '',
        headCommit: '',
        branch: '',
        navStack: [],
        remoteError: '',
        rightPanelFocused: false,
      };
    case 'SET_REMOTE_ERROR':
      return { ...s, remoteError: a.error };
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
    default:
      return s;
  }
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
