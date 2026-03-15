import { describe, it, expect } from 'vitest';
import { reducer, init, type LeftMode } from './state';

describe('reducer', () => {
  it('NAVIGATE sets currentPath, clears selectedFact, resets rightMode', () => {
    const s = { ...init, selectedFact: 'kb/foo', rightMode: 'fact' as const };
    const next = reducer(s, { type: 'NAVIGATE', path: 'kb/bar' });
    expect(next.currentPath).toBe('kb/bar');
    expect(next.selectedFact).toBeNull();
    expect(next.rightMode).toBe('summary');
  });

  it('SELECT_FACT sets selectedFact and rightMode to fact', () => {
    const next = reducer(init, { type: 'SELECT_FACT', path: 'kb/some-fact' });
    expect(next.selectedFact).toBe('kb/some-fact');
    expect(next.rightMode).toBe('fact');
  });

  it('PREVIEW_DIR clears selectedFact and sets previewPath', () => {
    const s = { ...init, selectedFact: 'kb/x' };
    const next = reducer(s, { type: 'PREVIEW_DIR', path: 'kb/subdir' });
    expect(next.selectedFact).toBeNull();
    expect(next.previewPath).toBe('kb/subdir');
  });

  it('GO_UP navigates to parent path', () => {
    const s = { ...init, currentPath: 'kb/science/physics' };
    const next = reducer(s, { type: 'GO_UP' });
    expect(next.currentPath).toBe('kb/science');
    expect(next.selectedFact).toBeNull();
  });

  it('GO_UP at root returns same state', () => {
    const s = { ...init, currentPath: 'kb' };
    const next = reducer(s, { type: 'GO_UP' });
    expect(next).toBe(s); // same reference
  });

  it('SEARCH sets searchQuery and clears similarTo', () => {
    const s = { ...init, similarTo: { path: 'x', text: 'y' } };
    const next = reducer(s, { type: 'SEARCH', query: 'hello' });
    expect(next.searchQuery).toBe('hello');
    expect(next.similarTo).toBeNull();
  });

  it('SIMILAR_SEARCH sets similarTo and clears searchQuery', () => {
    const s = { ...init, searchQuery: 'old' };
    const next = reducer(s, { type: 'SIMILAR_SEARCH', path: 'kb/a', text: 'some text' });
    expect(next.similarTo).toEqual({ path: 'kb/a', text: 'some text' });
    expect(next.searchQuery).toBe('');
  });

  it('CLEAR_SEARCH clears query, similarTo, and selectedFact', () => {
    const s = { ...init, searchQuery: 'q', similarTo: { path: 'x', text: 'y' }, selectedFact: 'kb/f' };
    const next = reducer(s, { type: 'CLEAR_SEARCH' });
    expect(next.searchQuery).toBe('');
    expect(next.similarTo).toBeNull();
    expect(next.selectedFact).toBeNull();
  });

  it('SHOW_HISTORY sets rightMode to history', () => {
    const next = reducer(init, { type: 'SHOW_HISTORY' });
    expect(next.rightMode).toBe('history');
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

  it('SET_STATUS sets head, branch, embeddingsEnabled, currentPath from ontologyRoot', () => {
    const next = reducer(init, { type: 'SET_STATUS', head: 'abc123', branch: 'main', embeddingsEnabled: true, ontologyRoot: 'knowledge' });
    expect(next.headCommit).toBe('abc123');
    expect(next.branch).toBe('main');
    expect(next.embeddingsEnabled).toBe(true);
    expect(next.currentPath).toBe('knowledge');
  });

  it('SET_STATUS keeps currentPath when ontologyRoot is empty', () => {
    const s = { ...init, currentPath: 'kb/custom' };
    const next = reducer(s, { type: 'SET_STATUS', head: 'abc', branch: 'main', embeddingsEnabled: false, ontologyRoot: '' });
    expect(next.currentPath).toBe('kb/custom');
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
    expect(next.consoleEntries[0].message).toBe('msg1'); // msg0 was trimmed
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
});

describe('history mode', () => {
  it('ENTER_HISTORY pushes to navStack and sets leftMode', () => {
    const s = reducer(init, { type: 'ENTER_HISTORY' });
    expect(s.leftMode).toBe('history');
    expect(s.navStack.length).toBe(1);
    expect(s.navStack[0].leftMode).toBe('browse');
  });

  it('EXIT_HISTORY resets to browse and clears historyCommit', () => {
    let s = reducer(init, { type: 'ENTER_HISTORY' });
    s = reducer(s, { type: 'SELECT_COMMIT', commit: 'abc123' });
    s = reducer(s, { type: 'EXIT_HISTORY' });
    expect(s.leftMode).toBe('browse');
    expect(s.historyCommit).toBeNull();
  });

  it('SELECT_COMMIT sets historyCommit', () => {
    const s = reducer(init, { type: 'SELECT_COMMIT', commit: 'abc123' });
    expect(s.historyCommit).toBe('abc123');
  });

  it('NAV_BACK pops navStack and restores state', () => {
    let s = reducer(init, { type: 'NAVIGATE', path: 'kb/tech' });
    s = reducer(s, { type: 'ENTER_HISTORY' });
    s = reducer(s, { type: 'NAV_BACK' });
    expect(s.leftMode).toBe('browse');
    expect(s.currentPath).toBe('kb/tech');
  });

  it('NAV_BACK on empty stack is no-op', () => {
    const s = reducer(init, { type: 'NAV_BACK' });
    expect(s).toBe(init);
  });

  it('navStack caps at 10 entries', () => {
    let s = init;
    for (let i = 0; i < 12; i++) {
      s = reducer(s, { type: 'NAVIGATE', path: `kb/p${i}` });
    }
    expect(s.navStack.length).toBe(10);
  });
});
