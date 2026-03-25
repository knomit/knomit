export type View = 'tree' | 'chrono' | 'history';

export interface FilterChip {
  category: 'domain' | 'entity' | 'type' | 'ep' | 'path';
  value: string;
}

export interface NavEntry {
  view: View;
  currentPath: string;
  selectedFact: string | null;
  filters: FilterChip[];
  historyCommit: string | null;
  freeText: string;
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
  currentPath: string;           // directory path for browse/scope
  selectedFact: string | null;
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
  historyCommit: string | null;
  navStack: NavEntry[];
  remoteError: string;
  rightPanelFocused: boolean;
}

export type Action =
  | { type: 'SET_VIEW'; view: View }
  | { type: 'NAVIGATE'; path: string }
  | { type: 'GO_UP' }
  | { type: 'SELECT_FACT'; path: string }
  | { type: 'ADD_FILTER'; chip: FilterChip }
  | { type: 'REMOVE_FILTER'; index: number }
  | { type: 'SET_FILTERS'; filters: FilterChip[] }
  | { type: 'SET_FREE_TEXT'; text: string }
  | { type: 'CLEAR_FILTERS' }
  | { type: 'NAV_BACK' }
  | { type: 'SELECT_COMMIT'; commit: string }
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
  | { type: 'BLUR_RIGHT_PANEL' };

export const init: AppState = {
  repo: 'knomit',
  view: 'tree',
  currentPath: 'kb',
  selectedFact: null,
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
  historyCommit: null,
  navStack: [],
  remoteError: '',
  rightPanelFocused: false,
};

function pushNav(s: AppState): NavEntry[] {
  const entry: NavEntry = {
    view: s.view,
    currentPath: s.currentPath,
    selectedFact: s.selectedFact,
    filters: [...s.filters],
    historyCommit: s.historyCommit,
    freeText: s.freeText,
  };
  const stack = [...s.navStack, entry];
  if (stack.length > 20) stack.shift();
  return stack;
}

export function currentPath(state: AppState): string {
  // Path filter chip takes precedence over currentPath (directory clicks add a path chip)
  const pathChip = state.filters.find(f => f.category === 'path');
  if (pathChip) return pathChip.value;
  return state.currentPath || state.ontologyRoot || 'kb';
}

export function reducer(s: AppState, a: Action): AppState {
  switch (a.type) {
    case 'SET_VIEW':
      return {
        ...s,
        view: a.view,
        historyCommit: a.view !== 'history' ? null : s.historyCommit,
        selectedFact: null,
        navStack: pushNav(s),
        rightPanelFocused: false,
      };
    case 'NAVIGATE':
      return {
        ...s,
        currentPath: a.path,
        selectedFact: null,
        navStack: pushNav(s),
        rightPanelFocused: false,
      };
    case 'GO_UP': {
      const parts = s.currentPath.split('/');
      if (parts.length <= 1) return s;
      return {
        ...s,
        currentPath: parts.slice(0, -1).join('/'),
        selectedFact: null,
        navStack: pushNav(s),
        rightPanelFocused: false,
      };
    }
    case 'SELECT_FACT':
      return { ...s, selectedFact: a.path, navStack: pushNav(s) };
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
      const removed = s.filters[a.index];
      const filters = s.filters.filter((_, i) => i !== a.index);
      // If removing the path chip, reset currentPath to ontology root
      const currentPathUpdate = removed?.category === 'path'
        ? { currentPath: s.ontologyRoot || 'kb' }
        : {};
      return { ...s, filters, ...currentPathUpdate, selectedFact: null, navStack: pushNav(s) };
    }
    case 'SET_FILTERS':
      return { ...s, filters: a.filters, navStack: pushNav(s) };
    case 'SET_FREE_TEXT':
      return { ...s, freeText: a.text };
    case 'CLEAR_FILTERS':
      return { ...s, filters: [], freeText: '', selectedFact: null, currentPath: s.ontologyRoot || 'kb', navStack: pushNav(s) };
    case 'NAV_BACK': {
      if (s.navStack.length === 0) return s;
      const prev = s.navStack[s.navStack.length - 1];
      return {
        ...s,
        view: prev.view,
        currentPath: prev.currentPath,
        selectedFact: prev.selectedFact,
        filters: prev.filters,
        historyCommit: prev.historyCommit,
        freeText: prev.freeText,
        navStack: s.navStack.slice(0, -1),
        rightPanelFocused: false,
      };
    }
    case 'SELECT_COMMIT':
      return { ...s, historyCommit: a.commit, selectedFact: null };
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
        currentPath: s.currentPath === 'kb' && a.ontologyRoot ? a.ontologyRoot : s.currentPath,
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
        currentPath: 'kb',
        selectedFact: null,
        filters: [],
        freeText: '',
        headCommit: '',
        branch: '',
        navStack: [],
        historyCommit: null,
        remoteError: '',
        rightPanelFocused: false,
      };
    case 'SET_REMOTE_ERROR':
      return { ...s, remoteError: a.error };
    case 'FOCUS_RIGHT_PANEL':
      return { ...s, rightPanelFocused: true };
    case 'BLUR_RIGHT_PANEL':
      return { ...s, rightPanelFocused: false };
    default:
      return s;
  }
}
