export type RightMode = 'summary' | 'fact' | 'history';

export interface AppState {
  currentPath: string;
  selectedFact: string | null;
  rightMode: RightMode;
  searchQuery: string;
  loading: boolean;
  syncing: boolean;
  headCommit: string;
  embeddingsEnabled: boolean;
}

export type Action =
  | { type: 'NAVIGATE'; path: string }
  | { type: 'SELECT_FACT'; path: string }
  | { type: 'SELECT_WORLD'; path: string }
  | { type: 'GO_UP' }
  | { type: 'SEARCH'; query: string }
  | { type: 'CLEAR_SEARCH' }
  | { type: 'SHOW_HISTORY' }
  | { type: 'SHOW_FACT' }
  | { type: 'SET_LOADING'; value: boolean }
  | { type: 'SET_SYNCING'; value: boolean }
  | { type: 'SET_STATUS'; head: string; embeddingsEnabled: boolean };

export const init: AppState = {
  currentPath: 'know',
  selectedFact: null,
  rightMode: 'summary',
  searchQuery: '',
  loading: false,
  syncing: false,
  headCommit: '',
  embeddingsEnabled: false,
};

export function reducer(s: AppState, a: Action): AppState {
  switch (a.type) {
    case 'NAVIGATE': return { ...s, currentPath: a.path, selectedFact: null, rightMode: 'summary', searchQuery: '' };
    case 'SELECT_FACT': return { ...s, selectedFact: a.path, rightMode: 'fact' };
    case 'SELECT_WORLD': return { ...s, selectedFact: null, rightMode: 'summary' };
    case 'GO_UP': {
      const parts = s.currentPath.split('/');
      if (parts.length <= 1) return s;
      return { ...s, currentPath: parts.slice(0, -1).join('/'), selectedFact: null, rightMode: 'summary' };
    }
    case 'SEARCH': return { ...s, searchQuery: a.query };
    case 'CLEAR_SEARCH': return { ...s, searchQuery: '', selectedFact: null, rightMode: 'summary' };
    case 'SHOW_HISTORY': return { ...s, rightMode: 'history' };
    case 'SHOW_FACT': return { ...s, rightMode: 'fact' };
    case 'SET_LOADING': return { ...s, loading: a.value };
    case 'SET_SYNCING': return { ...s, syncing: a.value };
    case 'SET_STATUS': return { ...s, headCommit: a.head, embeddingsEnabled: a.embeddingsEnabled };
    default: return s;
  }
}
