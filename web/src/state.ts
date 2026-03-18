export type RightMode = 'summary' | 'fact' | 'history';
export type LeftMode = 'browse' | 'history';

export interface NavEntry {
  currentPath: string;
  selectedFact: string | null;
  leftMode: LeftMode;
  historyCommit: string | null;
  rightMode: RightMode;
  searchQuery: string;
}

export interface ConsoleEntry {
  id: number;
  time: number; // Date.now()
  level: 'info' | 'error';
  message: string;
}

export interface AppState {
  repo: string;
  currentPath: string;
  selectedFact: string | null;
  previewPath: string | null; // directory being previewed in summary panel without navigating
  rightMode: RightMode;
  searchQuery: string;
  similarTo: { path: string; text: string } | null; // fact for similarity search
  loading: boolean;
  tasks: Record<string, { status: 'idle' | 'running' | 'done' | 'error'; message: string }>;
  headCommit: string;
  branch: string;
  embeddingsEnabled: boolean;
  statusMessage: string;
  consoleEntries: ConsoleEntry[];
  consoleOpen: boolean;
  consoleHeight: number; // pixels
  leftMode: LeftMode;
  historyCommit: string | null;
  historyFocusPath: string | null; // in history mode: load this specific path (not commitDetail auto-select)
  navStack: NavEntry[];
  remoteError: string; // latest sync/push error, empty = ok
  rightPanelFocused: boolean;
}

export type Action =
  | { type: 'NAVIGATE'; path: string }
  | { type: 'SELECT_FACT'; path: string }
  | { type: 'PREVIEW_DIR'; path: string }
  | { type: 'SELECT_WORLD'; path: string }
  | { type: 'GO_UP' }
  | { type: 'SEARCH'; query: string }
  | { type: 'SIMILAR_SEARCH'; path: string; text: string }
  | { type: 'CLEAR_SEARCH' }
  | { type: 'SHOW_HISTORY' }
  | { type: 'SHOW_FACT' }
  | { type: 'SET_LOADING'; value: boolean }
  | { type: 'SET_TASK'; op: string; status: 'idle' | 'running' | 'done' | 'error'; message: string }
  | { type: 'SET_STATUS'; head: string; branch: string; embeddingsEnabled: boolean; ontologyRoot: string }
  | { type: 'SET_HEAD'; head: string }
  | { type: 'SET_STATUS_MESSAGE'; message: string }
  | { type: 'CONSOLE_LOG'; level: 'info' | 'error'; message: string }
  | { type: 'CONSOLE_TOGGLE' }
  | { type: 'CONSOLE_SET_HEIGHT'; height: number }
  | { type: 'ENTER_HISTORY' }
  | { type: 'EXIT_HISTORY' }
  | { type: 'FACT_HISTORY'; factPath: string; commit?: string }
  | { type: 'SELECT_COMMIT'; commit: string }
  | { type: 'NAV_BACK' }
  | { type: 'SET_REPO'; repo: string }
  | { type: 'SET_REMOTE_ERROR'; error: string }
  | { type: 'OPEN_FACT'; path: string }
  | { type: 'HISTORY_OPEN_PATH'; path: string }
  | { type: 'FOCUS_RIGHT_PANEL' }
  | { type: 'BLUR_RIGHT_PANEL' };

export const init: AppState = {
  repo: 'knomit',
  currentPath: 'kb',
  selectedFact: null,
  previewPath: null,
  rightMode: 'summary',
  searchQuery: '',
  similarTo: null,
  loading: false,
  tasks: { sync: { status: 'idle', message: '' }, synth: { status: 'idle', message: '' } },
  headCommit: '',
  branch: '',
  embeddingsEnabled: false,
  statusMessage: '',
  consoleEntries: [],
  consoleOpen: false,
  consoleHeight: 200,
  leftMode: 'browse' as LeftMode,
  remoteError: '',
  historyCommit: null,
  historyFocusPath: null,
  navStack: [],
  rightPanelFocused: false,
};

function pushNav(s: AppState): NavEntry[] {
  const entry: NavEntry = {
    currentPath: s.currentPath,
    selectedFact: s.selectedFact,
    leftMode: s.leftMode,
    historyCommit: s.historyCommit,
    rightMode: s.rightMode,
    searchQuery: s.searchQuery,
  };
  const stack = [...s.navStack, entry];
  if (stack.length > 10) stack.shift();
  return stack;
}

export function reducer(s: AppState, a: Action): AppState {
  switch (a.type) {
    case 'NAVIGATE': return { ...s, currentPath: a.path, selectedFact: null, previewPath: null, rightMode: 'summary', searchQuery: '', similarTo: null, historyFocusPath: null, navStack: pushNav(s), rightPanelFocused: false };
    case 'SELECT_FACT': return { ...s, selectedFact: a.path, previewPath: null, rightMode: 'fact', navStack: pushNav(s) };
    case 'PREVIEW_DIR': return { ...s, selectedFact: null, previewPath: a.path, rightMode: 'summary' };
    case 'SELECT_WORLD': return { ...s, selectedFact: null, previewPath: null, rightMode: 'summary' };
    case 'GO_UP': {
      const parts = s.currentPath.split('/');
      if (parts.length <= 1) return s;
      return { ...s, currentPath: parts.slice(0, -1).join('/'), selectedFact: null, previewPath: null, rightMode: 'summary' };
    }
    case 'SEARCH': return { ...s, searchQuery: a.query, similarTo: null, previewPath: null, navStack: pushNav(s), rightPanelFocused: false };
    case 'SIMILAR_SEARCH': return { ...s, similarTo: { path: a.path, text: a.text }, searchQuery: '', previewPath: null, rightPanelFocused: false };
    case 'CLEAR_SEARCH': return { ...s, searchQuery: '', similarTo: null, selectedFact: null, previewPath: null, rightMode: 'summary' };
    case 'SHOW_HISTORY': return { ...s, rightMode: 'history', rightPanelFocused: false };
    case 'SHOW_FACT': return { ...s, rightMode: 'fact' };
    case 'SET_LOADING': return { ...s, loading: a.value };
    case 'SET_TASK': {
      const cur = s.tasks[a.op];
      if (cur && cur.status === a.status && cur.message === a.message) return s;
      return { ...s, tasks: { ...s.tasks, [a.op]: { status: a.status, message: a.message } } };
    }
    case 'SET_STATUS': return { ...s, headCommit: a.head, branch: a.branch, embeddingsEnabled: a.embeddingsEnabled, currentPath: a.ontologyRoot || s.currentPath };
    case 'SET_HEAD': return { ...s, headCommit: a.head };
    case 'SET_STATUS_MESSAGE': return { ...s, statusMessage: a.message };
    case 'CONSOLE_LOG': {
      const entry: ConsoleEntry = { id: Date.now() + Math.random(), time: Date.now(), level: a.level, message: a.message };
      const entries = [...s.consoleEntries, entry];
      if (entries.length > 500) entries.splice(0, entries.length - 500);
      return { ...s, consoleEntries: entries };
    }
    case 'CONSOLE_TOGGLE': return { ...s, consoleOpen: !s.consoleOpen };
    case 'CONSOLE_SET_HEIGHT': return { ...s, consoleHeight: Math.max(80, Math.min(a.height, 600)) };
    case 'ENTER_HISTORY': return { ...s, leftMode: 'history' as LeftMode, navStack: pushNav(s), rightPanelFocused: false };
    case 'EXIT_HISTORY': {
      // If currentPath is a fact (.md), restore to its parent directory and keep the fact selected
      if (s.currentPath.endsWith('.md')) {
        const parentDir = s.currentPath.split('/').slice(0, -1).join('/') || s.currentPath;
        return { ...s, currentPath: parentDir, selectedFact: s.currentPath, leftMode: 'browse' as LeftMode, historyCommit: null, historyFocusPath: null, rightMode: 'fact', rightPanelFocused: false };
      }
      return { ...s, leftMode: 'browse' as LeftMode, historyCommit: null, historyFocusPath: null, rightPanelFocused: false };
    }
    case 'FACT_HISTORY':
      return { ...s, currentPath: a.factPath, selectedFact: a.factPath, leftMode: 'history' as LeftMode, historyCommit: a.commit || null, historyFocusPath: a.factPath, navStack: pushNav(s), rightPanelFocused: false };
    case 'SELECT_COMMIT': return { ...s, historyCommit: a.commit };
    case 'OPEN_FACT': {
      const parts = a.path.split('/');
      const parentDir = parts.slice(0, -1).join('/') || s.currentPath;
      return { ...s, currentPath: parentDir, selectedFact: a.path, rightMode: 'fact', historyCommit: null, historyFocusPath: null, leftMode: 'browse' as LeftMode, navStack: pushNav(s), rightPanelFocused: false };
    }
    case 'HISTORY_OPEN_PATH': return { ...s, currentPath: a.path, historyFocusPath: a.path, navStack: pushNav(s), rightPanelFocused: false };
    case 'NAV_BACK': {
      if (s.navStack.length === 0) return s;
      const prev = s.navStack[s.navStack.length - 1];
      return { ...s, ...prev, navStack: s.navStack.slice(0, -1), rightPanelFocused: false };
    }
    case 'SET_REPO': return {
      ...s,
      repo: a.repo,
      currentPath: 'kb',
      selectedFact: null,
      previewPath: null,
      rightMode: 'summary',
      searchQuery: '',
      similarTo: null,
      headCommit: '',
      branch: '',
      navStack: [],
      leftMode: 'browse' as LeftMode,
      historyCommit: null,
      remoteError: '',
      rightPanelFocused: false,
    };
    case 'SET_REMOTE_ERROR': return { ...s, remoteError: a.error };
    case 'FOCUS_RIGHT_PANEL': return { ...s, rightPanelFocused: true };
    case 'BLUR_RIGHT_PANEL': return { ...s, rightPanelFocused: false };
    default: return s;
  }
}
