export type View = 'tree' | 'chrono' | 'history';

export interface FilterChip {
  category: 'domain' | 'entity' | 'type' | 'ep' | 'path';
  value: string;
}

export interface NavEntry {
  repo: string;
  branch: string;
  view: View;
  filters: FilterChip[];
  freeText: string;
  historyCommit: string | null;
  factPath: string | null;
  factCommit: string | null;
}

export interface ConsoleEntry {
  id: number;
  time: number; // Date.now()
  level: 'info' | 'error';
  message: string;
}

export interface AppState {
  repo: string;
  view: View;
  historyCommit: string | null;  // history mode: commit selected in timeline
  factPath: string | null;       // right panel: fact to display (all modes)
  factCommit: string | null;     // right panel: commit to show fact at (null = HEAD)
  filters: FilterChip[];
  freeText: string;              // unprefixed search text
  loading: boolean;
  tasks: Record<string, { status: 'idle' | 'running' | 'done' | 'error'; message: string }>;
  headCommit: string;
  branch: string;
  embeddingsEnabled: boolean;
  ontologyRoot: string;
  statusMessage: string;
  consoleEntries: ConsoleEntry[];
  consoleOpen: boolean;
  consoleHeight: number;
  navStack: NavEntry[];
  remoteError: string;
  rightPanelFocused: boolean;
}

export type Action =
  | { type: 'SET_VIEW'; view: View }
  | { type: 'NAVIGATE'; path: string }
  | { type: 'GO_UP' }
  | { type: 'ADD_FILTER'; chip: FilterChip }
  | { type: 'REMOVE_FILTER'; index: number }
  | { type: 'SET_FILTERS'; filters: FilterChip[] }
  | { type: 'SET_FREE_TEXT'; text: string }
  | { type: 'CLEAR_FILTERS' }
  | { type: 'NAV_BACK' }
  | { type: 'SET_LOADING'; value: boolean }
  | { type: 'SET_TASK'; op: string; status: 'idle' | 'running' | 'done' | 'error'; message: string }
  | { type: 'SET_STATUS'; head: string; branch: string; embeddingsEnabled: boolean; ontologyRoot: string }
  | { type: 'SET_HEAD'; head: string }
  | { type: 'SET_STATUS_MESSAGE'; message: string }
  | { type: 'CONSOLE_LOG'; level: 'info' | 'error'; message: string }
  | { type: 'CONSOLE_TOGGLE' }
  | { type: 'CONSOLE_SET_HEIGHT'; height: number }
  | { type: 'SET_REPO'; repo: string }
  | { type: 'SET_REMOTE_ERROR'; error: string }
  | { type: 'FOCUS_RIGHT_PANEL' }
  | { type: 'BLUR_RIGHT_PANEL' }
  | { type: 'APPLY_NAV'; view: View; historyCommit: string | null; factPath: string | null; factCommit: string | null; filters?: FilterChip[]; freeText?: string }
  | { type: 'FACT_LOADED'; commit: string };

export const init: AppState = {
  repo: 'knomit',
  view: 'tree',
  historyCommit: null,
  factPath: null,
  factCommit: null,
  filters: [],
  freeText: '',
  loading: false,
  tasks: { sync: { status: 'idle', message: '' }, synth: { status: 'idle', message: '' } },
  headCommit: '',
  branch: '',
  embeddingsEnabled: false,
  ontologyRoot: 'kb',
  statusMessage: '',
  consoleEntries: [],
  consoleOpen: false,
  consoleHeight: 200,
  navStack: [],
  remoteError: '',
  rightPanelFocused: false,
};

function pushNav(s: AppState): NavEntry[] {
  const entry: NavEntry = {
    repo: s.repo,
    branch: s.branch,
    view: s.view,
    filters: [...s.filters],
    freeText: s.freeText,
    historyCommit: s.historyCommit,
    factPath: s.factPath,
    factCommit: s.factCommit,
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
    case 'SET_VIEW': {
      // Tree ↔ Chrono: keep all filters (same data, different presentation).
      // Tree/Chrono ↔ History: clear all filters except path, clear freeText.
      const crossingBoundary =
        (s.view === 'history' && a.view !== 'history') ||
        (s.view !== 'history' && a.view === 'history');
      return {
        ...s,
        view: a.view,
        historyCommit: a.view === 'history' ? s.historyCommit : null,
        factCommit: a.view === 'history' ? s.factCommit : null,
        // factPath preserved across view changes (same fact shown in new mode)
        filters: crossingBoundary ? s.filters.filter(f => f.category === 'path') : s.filters,
        freeText: crossingBoundary ? '' : s.freeText,
        navStack: pushNav(s),
        rightPanelFocused: false,
      };
    }
    case 'NAVIGATE':
      return {
        ...s,
        filters: replacePathChip(s.filters, a.path),
        historyCommit: null,
        factPath: null,
        factCommit: null,
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
        historyCommit: null,
        factPath: null,
        factCommit: null,
        navStack: pushNav(s),
        rightPanelFocused: false,
      };
    }
    case 'ADD_FILTER': {
      let filters: FilterChip[];
      if (a.chip.category === 'path') {
        // Replace existing path chip
        filters = [...s.filters.filter(f => f.category !== 'path'), a.chip];
      } else {
        filters = [...s.filters, a.chip];
      }
      return { ...s, filters, navStack: pushNav(s) };
    }
    case 'REMOVE_FILTER': {
      const filters = s.filters.filter((_, i) => i !== a.index);
      return { ...s, filters, historyCommit: null, factPath: null, factCommit: null, navStack: pushNav(s) };
    }
    case 'SET_FILTERS':
      return { ...s, filters: a.filters, navStack: pushNav(s) };
    case 'SET_FREE_TEXT':
      return { ...s, freeText: a.text };
    case 'CLEAR_FILTERS':
      return { ...s, filters: [], freeText: '', historyCommit: null, factPath: null, factCommit: null, navStack: pushNav(s) };
    case 'NAV_BACK': {
      if (s.navStack.length === 0) return s;
      const prev = s.navStack[s.navStack.length - 1];
      // If repo changed, treat as full reset
      if (prev.repo !== s.repo) {
        return {
          ...s,
          repo: prev.repo,
          view: 'tree',
          historyCommit: null,
          factPath: null,
          factCommit: null,
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
        historyCommit: prev.historyCommit,
        factPath: prev.factPath,
        factCommit: prev.factCommit,
        filters: prev.filters,
        freeText: prev.freeText,
        navStack: s.navStack.slice(0, -1),
        rightPanelFocused: false,
      };
    }
    case 'SET_LOADING':
      return { ...s, loading: a.value };
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
      };
    case 'SET_HEAD':
      return { ...s, headCommit: a.head };
    case 'SET_STATUS_MESSAGE':
      return { ...s, statusMessage: a.message };
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
        view: 'tree',
        historyCommit: null,
        factPath: null,
        factCommit: null,
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
    case 'APPLY_NAV': {
      const crossingBoundary =
        (s.view === 'history' && a.view !== 'history') ||
        (s.view !== 'history' && a.view === 'history');
      return {
        ...s,
        view: a.view,
        historyCommit: a.historyCommit,
        factPath: a.factPath,
        factCommit: a.factCommit,
        filters: a.filters !== undefined ? a.filters : crossingBoundary ? s.filters.filter(f => f.category === 'path') : s.filters,
        freeText: a.freeText !== undefined ? a.freeText : crossingBoundary ? '' : s.freeText,
        navStack: pushNav(s),
        rightPanelFocused: false,
      };
    }
    case 'FACT_LOADED':
      return { ...s, factCommit: a.commit };
    default:
      return s;
  }
}
