export type RightMode = 'summary' | 'fact' | 'history';

export interface AppState {
  currentPath: string;
  selectedFact: string | null;
  previewPath: string | null; // directory being previewed in summary panel without navigating
  rightMode: RightMode;
  searchQuery: string;
  loading: boolean;
  tasks: Record<string, { status: 'idle' | 'running' | 'done' | 'error'; message: string }>;
  headCommit: string;
  branch: string;
  embeddingsEnabled: boolean;
  statusMessage: string;
}

export type Action =
  | { type: 'NAVIGATE'; path: string }
  | { type: 'SELECT_FACT'; path: string }
  | { type: 'PREVIEW_DIR'; path: string }
  | { type: 'SELECT_WORLD'; path: string }
  | { type: 'GO_UP' }
  | { type: 'SEARCH'; query: string }
  | { type: 'CLEAR_SEARCH' }
  | { type: 'SHOW_HISTORY' }
  | { type: 'SHOW_FACT' }
  | { type: 'SET_LOADING'; value: boolean }
  | { type: 'SET_TASK'; op: string; status: 'idle' | 'running' | 'done' | 'error'; message: string }
  | { type: 'SET_STATUS'; head: string; branch: string; embeddingsEnabled: boolean }
  | { type: 'SET_HEAD'; head: string }
  | { type: 'SET_STATUS_MESSAGE'; message: string };

export const init: AppState = {
  currentPath: 'know',
  selectedFact: null,
  previewPath: null,
  rightMode: 'summary',
  searchQuery: '',
  loading: false,
  tasks: { sync: { status: 'idle', message: '' }, synth: { status: 'idle', message: '' } },
  headCommit: '',
  branch: '',
  embeddingsEnabled: false,
  statusMessage: '',
};

export function reducer(s: AppState, a: Action): AppState {
  switch (a.type) {
    case 'NAVIGATE': return { ...s, currentPath: a.path, selectedFact: null, previewPath: null, rightMode: 'summary', searchQuery: '' };
    case 'SELECT_FACT': return { ...s, selectedFact: a.path, previewPath: null, rightMode: 'fact' };
    case 'PREVIEW_DIR': return { ...s, selectedFact: null, previewPath: a.path, rightMode: 'summary' };
    case 'SELECT_WORLD': return { ...s, selectedFact: null, previewPath: null, rightMode: 'summary' };
    case 'GO_UP': {
      const parts = s.currentPath.split('/');
      if (parts.length <= 1) return s;
      return { ...s, currentPath: parts.slice(0, -1).join('/'), selectedFact: null, previewPath: null, rightMode: 'summary' };
    }
    case 'SEARCH': return { ...s, searchQuery: a.query, previewPath: null };
    case 'CLEAR_SEARCH': return { ...s, searchQuery: '', selectedFact: null, previewPath: null, rightMode: 'summary' };
    case 'SHOW_HISTORY': return { ...s, rightMode: 'history' };
    case 'SHOW_FACT': return { ...s, rightMode: 'fact' };
    case 'SET_LOADING': return { ...s, loading: a.value };
    case 'SET_TASK': return { ...s, tasks: { ...s.tasks, [a.op]: { status: a.status, message: a.message } } };
    case 'SET_STATUS': return { ...s, headCommit: a.head, branch: a.branch, embeddingsEnabled: a.embeddingsEnabled };
    case 'SET_HEAD': return { ...s, headCommit: a.head };
    case 'SET_STATUS_MESSAGE': return { ...s, statusMessage: a.message };
    default: return s;
  }
}
