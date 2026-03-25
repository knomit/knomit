import { describe, it, expect } from 'vitest';
import { reducer, init, currentPath } from './state';
import type { AppState, FilterChip } from './state';

describe('reducer — view', () => {
  it('SET_VIEW changes view and pushes to navStack', () => {
    const s = reducer(init, { type: 'SET_VIEW', view: 'chrono' });
    expect(s.view).toBe('chrono');
    expect(s.navStack.length).toBe(1);
    expect(s.navStack[0].view).toBe('tree');
  });

  it('SET_VIEW clears selectedFact', () => {
    const s = { ...init, selectedFact: 'kb/foo.md' };
    const next = reducer(s, { type: 'SET_VIEW', view: 'chrono' });
    expect(next.selectedFact).toBeNull();
  });

  it('SET_VIEW clears historyCommit when leaving history', () => {
    const s = { ...init, view: 'history' as const, historyCommit: 'abc123' };
    const next = reducer(s, { type: 'SET_VIEW', view: 'tree' });
    expect(next.historyCommit).toBeNull();
  });

  it('SET_VIEW keeps historyCommit when staying in history', () => {
    const s = { ...init, view: 'history' as const, historyCommit: 'abc123' };
    const next = reducer(s, { type: 'SET_VIEW', view: 'history' });
    expect(next.historyCommit).toBe('abc123');
  });

  it('SET_VIEW clears rightPanelFocused', () => {
    const s = { ...init, rightPanelFocused: true };
    const next = reducer(s, { type: 'SET_VIEW', view: 'chrono' });
    expect(next.rightPanelFocused).toBe(false);
  });
});

describe('reducer — filters', () => {
  it('ADD_FILTER appends chip and pushes nav', () => {
    const chip: FilterChip = { category: 'domain', value: 'tech' };
    const s = reducer(init, { type: 'ADD_FILTER', chip });
    expect(s.filters).toHaveLength(1);
    expect(s.filters[0]).toEqual(chip);
    expect(s.navStack.length).toBe(1);
  });

  it('ADD_FILTER with path category replaces existing path chip', () => {
    const first: FilterChip = { category: 'path', value: 'kb/tech' };
    const second: FilterChip = { category: 'path', value: 'kb/science' };
    let s = reducer(init, { type: 'ADD_FILTER', chip: first });
    s = reducer(s, { type: 'ADD_FILTER', chip: second });
    const pathChips = s.filters.filter(f => f.category === 'path');
    expect(pathChips).toHaveLength(1);
    expect(pathChips[0].value).toBe('kb/science');
  });

  it('ADD_FILTER with path category keeps other chips', () => {
    let s = reducer(init, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'tech' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/tech' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/science' } });
    expect(s.filters.find(f => f.category === 'domain')?.value).toBe('tech');
    expect(s.filters.filter(f => f.category === 'path')).toHaveLength(1);
  });

  it('REMOVE_FILTER removes chip at given index and pushes nav', () => {
    let s = reducer(init, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'a' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'entity', value: 'b' } });
    const before = s.navStack.length;
    s = reducer(s, { type: 'REMOVE_FILTER', index: 0 });
    expect(s.filters).toHaveLength(1);
    expect(s.filters[0].value).toBe('b');
    expect(s.navStack.length).toBe(before + 1);
  });

  it('SET_FILTERS replaces all filters and pushes nav', () => {
    const chips: FilterChip[] = [{ category: 'type', value: 'fact' }];
    const s = reducer(init, { type: 'SET_FILTERS', filters: chips });
    expect(s.filters).toEqual(chips);
    expect(s.navStack.length).toBe(1);
  });

  it('SET_FREE_TEXT sets freeText without pushing nav', () => {
    const s = reducer(init, { type: 'SET_FREE_TEXT', text: 'hello' });
    expect(s.freeText).toBe('hello');
    expect(s.navStack.length).toBe(0);
  });

  it('CLEAR_FILTERS clears filters, freeText, selectedFact and pushes nav', () => {
    let s: AppState = { ...init, filters: [{ category: 'domain', value: 'tech' }], freeText: 'q', selectedFact: 'kb/f.md' };
    s = reducer(s, { type: 'CLEAR_FILTERS' });
    expect(s.filters).toHaveLength(0);
    expect(s.freeText).toBe('');
    expect(s.selectedFact).toBeNull();
    expect(s.navStack.length).toBe(1);
  });
});

describe('reducer — nav', () => {
  it('NAV_BACK restores previous view/selectedFact/filters/historyCommit/freeText', () => {
    let s: AppState = { ...init, selectedFact: 'kb/a.md', freeText: 'q' };
    s = reducer(s, { type: 'SET_VIEW', view: 'chrono' });
    s = reducer(s, { type: 'NAV_BACK' });
    expect(s.view).toBe('tree');
    // SET_VIEW pushes the state before it cleared selectedFact, so restoring goes back to it
    expect(s.selectedFact).toBe('kb/a.md');
    expect(s.freeText).toBe('q');
    expect(s.navStack.length).toBe(0);
  });

  it('NAV_BACK restores filters from stack', () => {
    let s = reducer(init, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'tech' } });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'entity', value: 'ai' } });
    const withFilters = s;
    s = reducer(s, { type: 'CLEAR_FILTERS' });
    s = reducer(s, { type: 'NAV_BACK' });
    expect(s.filters).toEqual(withFilters.filters);
  });

  it('NAV_BACK on empty stack is no-op', () => {
    const s = reducer(init, { type: 'NAV_BACK' });
    expect(s).toBe(init);
  });

  it('NAV_BACK clears rightPanelFocused', () => {
    let s = reducer(init, { type: 'SET_VIEW', view: 'chrono' });
    s = reducer({ ...s, rightPanelFocused: true }, { type: 'NAV_BACK' });
    expect(s.rightPanelFocused).toBe(false);
  });

  it('nav stack caps at 20 entries', () => {
    let s = init;
    for (let i = 0; i < 22; i++) {
      s = reducer(s, { type: 'SET_VIEW', view: i % 2 === 0 ? 'chrono' : 'tree' });
    }
    expect(s.navStack.length).toBe(20);
  });
});

describe('currentPath()', () => {
  it('returns currentPath from state', () => {
    const s = { ...init, currentPath: 'kb/tech' };
    expect(currentPath(s)).toBe('kb/tech');
  });

  it('returns ontologyRoot when currentPath is empty', () => {
    const s = { ...init, currentPath: '', ontologyRoot: 'knowledge' };
    expect(currentPath(s)).toBe('knowledge');
  });

  it('returns kb fallback when both empty', () => {
    const s = { ...init, currentPath: '', ontologyRoot: '' };
    expect(currentPath(s)).toBe('kb');
  });
});

describe('reducer — NAVIGATE', () => {
  it('sets currentPath and pushes nav', () => {
    const s = reducer(init, { type: 'NAVIGATE', path: 'kb/tech' });
    expect(s.currentPath).toBe('kb/tech');
    expect(s.selectedFact).toBeNull();
    expect(s.navStack.length).toBe(1);
  });
});

describe('reducer — GO_UP', () => {
  it('removes last path segment', () => {
    const s0 = { ...init, currentPath: 'kb/tech/go' };
    const s = reducer(s0, { type: 'GO_UP' });
    expect(s.currentPath).toBe('kb/tech');
  });

  it('does nothing at root', () => {
    const s0 = { ...init, currentPath: 'kb' };
    const s = reducer(s0, { type: 'GO_UP' });
    expect(s.currentPath).toBe('kb');
  });
});

describe('reducer — SELECT_FACT', () => {
  it('sets selectedFact and pushes nav', () => {
    const s = reducer(init, { type: 'SELECT_FACT', path: 'kb/foo.md' });
    expect(s.selectedFact).toBe('kb/foo.md');
    expect(s.navStack.length).toBe(1);
  });
});

describe('reducer — SELECT_COMMIT', () => {
  it('sets historyCommit without pushing nav', () => {
    const s = reducer(init, { type: 'SELECT_COMMIT', commit: 'abc123' });
    expect(s.historyCommit).toBe('abc123');
    expect(s.navStack.length).toBe(0);
  });
});

describe('reducer — SET_REPO', () => {
  it('resets navigation state when switching repos', () => {
    let s = reducer(init, { type: 'SELECT_FACT', path: 'kb/deep/fact.md' });
    s = reducer(s, { type: 'ADD_FILTER', chip: { category: 'domain', value: 'tech' } });
    s = reducer(s, { type: 'SET_VIEW', view: 'history' });
    s = reducer(s, { type: 'SET_REPO', repo: 'work' });
    expect(s.repo).toBe('work');
    expect(s.view).toBe('tree');
    expect(s.selectedFact).toBeNull();
    expect(s.filters).toHaveLength(0);
    expect(s.freeText).toBe('');
    expect(s.navStack).toHaveLength(0);
    expect(s.historyCommit).toBeNull();
    expect(s.headCommit).toBe('');
    expect(s.branch).toBe('');
    expect(s.remoteError).toBe('');
    expect(s.rightPanelFocused).toBe(false);
  });

  it('init has repo set to knomit', () => {
    expect(init.repo).toBe('knomit');
  });
});

describe('reducer — shared infrastructure', () => {
  it('SET_LOADING updates loading flag', () => {
    expect(reducer(init, { type: 'SET_LOADING', value: true }).loading).toBe(true);
  });

  it('SET_TASK updates task status', () => {
    const next = reducer(init, { type: 'SET_TASK', op: 'sync', status: 'running', message: 'syncing' });
    expect(next.tasks.sync).toEqual({ status: 'running', message: 'syncing' });
  });

  it('SET_TASK returns same reference when no change', () => {
    const s = { ...init, tasks: { ...init.tasks, sync: { status: 'idle' as const, message: '' } } };
    const next = reducer(s, { type: 'SET_TASK', op: 'sync', status: 'idle', message: '' });
    expect(next).toBe(s);
  });

  it('SET_STATUS sets head, branch, embeddingsEnabled, ontologyRoot', () => {
    const next = reducer(init, { type: 'SET_STATUS', head: 'abc123', branch: 'main', embeddingsEnabled: true, ontologyRoot: 'knowledge' });
    expect(next.headCommit).toBe('abc123');
    expect(next.branch).toBe('main');
    expect(next.embeddingsEnabled).toBe(true);
    expect(next.ontologyRoot).toBe('knowledge');
  });

  it('SET_STATUS keeps ontologyRoot when empty string provided', () => {
    const s = { ...init, ontologyRoot: 'kb/custom' };
    const next = reducer(s, { type: 'SET_STATUS', head: 'abc', branch: 'main', embeddingsEnabled: false, ontologyRoot: '' });
    expect(next.ontologyRoot).toBe('kb/custom');
  });

  it('SET_HEAD only updates headCommit', () => {
    const next = reducer(init, { type: 'SET_HEAD', head: 'def456' });
    expect(next.headCommit).toBe('def456');
    expect(next.branch).toBe(init.branch);
  });

  it('CONSOLE_LOG adds entry and caps at 500', () => {
    const next = reducer(init, { type: 'CONSOLE_LOG', level: 'info', message: 'hello' });
    expect(next.consoleEntries).toHaveLength(1);
    expect(next.consoleEntries[0].message).toBe('hello');
    expect(next.consoleEntries[0].level).toBe('info');
  });

  it('CONSOLE_LOG trims entries beyond 500', () => {
    const entries = Array.from({ length: 500 }, (_, i) => ({
      id: i, time: i, level: 'info' as const, message: `msg${i}`,
    }));
    const s = { ...init, consoleEntries: entries };
    const next = reducer(s, { type: 'CONSOLE_LOG', level: 'error', message: 'overflow' });
    expect(next.consoleEntries).toHaveLength(500);
    expect(next.consoleEntries[499].message).toBe('overflow');
    expect(next.consoleEntries[0].message).toBe('msg1');
  });

  it('CONSOLE_TOGGLE flips consoleOpen', () => {
    expect(reducer(init, { type: 'CONSOLE_TOGGLE' }).consoleOpen).toBe(true);
    const open = { ...init, consoleOpen: true };
    expect(reducer(open, { type: 'CONSOLE_TOGGLE' }).consoleOpen).toBe(false);
  });

  it('CONSOLE_SET_HEIGHT clamps between 80 and 600', () => {
    expect(reducer(init, { type: 'CONSOLE_SET_HEIGHT', height: 50 }).consoleHeight).toBe(80);
    expect(reducer(init, { type: 'CONSOLE_SET_HEIGHT', height: 300 }).consoleHeight).toBe(300);
    expect(reducer(init, { type: 'CONSOLE_SET_HEIGHT', height: 900 }).consoleHeight).toBe(600);
  });

  it('SET_REMOTE_ERROR sets remoteError', () => {
    const s = reducer(init, { type: 'SET_REMOTE_ERROR', error: 'auth failed' });
    expect(s.remoteError).toBe('auth failed');
  });

  it('FOCUS_RIGHT_PANEL sets rightPanelFocused to true', () => {
    expect(reducer(init, { type: 'FOCUS_RIGHT_PANEL' }).rightPanelFocused).toBe(true);
  });

  it('BLUR_RIGHT_PANEL sets rightPanelFocused to false', () => {
    const s = reducer({ ...init, rightPanelFocused: true }, { type: 'BLUR_RIGHT_PANEL' });
    expect(s.rightPanelFocused).toBe(false);
  });
});
