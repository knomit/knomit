// Test 7 (plan 7.7): the FactSet entry control sits beside the count in every
// SEARCH list (relevance and recent, a chip-driven list included, repo and
// lens), is absent in the path tree, is disabled in history, and opens the
// FactSet of the list's CURRENT search, built from the very parameters the
// list just sent.
//
// Sabotage, one run each: show the control in Path mode; drop the history
// check (the control is enabled in history); build the FactSet from the list
// state without the chips (the frozen params lose `type`).

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';
import type { AppState } from './state';
import type { Lens } from './api';
import { factsetView } from './factsetOptions';

const calls: { fn: string; args: unknown[] }[] = [];
vi.mock('./api', () => {
  const rec = (fn: string, value: unknown) => vi.fn(async (...args: unknown[]) => { calls.push({ fn, args }); return value; });
  return {
    api: {
      browse: rec('browse', { children: [{ name: 'architecture', is_dir: true }] }),
      recent: rec('recent', { facts: [{ path: 'kb/a.md', title: 'A', type: 'observation', committed_at: 1 }], total: 1 }),
      search: rec('search', { results: [{ path: 'kb/a.md', title: 'A', type: 'observation' }] }),
      lensBrowse: rec('lensBrowse', { children: [] }),
      listLensFacts: rec('listLensFacts', { facts: [], total: 0 }),
      lensSearch: rec('lensSearch', []),
    },
  };
});

const lens: Lens = { name: 'eng', write: { uid: 'u1', name: 'core' }, reads: [{ uid: 'u1', name: 'core' }, { uid: 'u2', name: 'docs' }] };
const repoState = (over: Partial<AppState> = {}): AppState => ({
  ...init, repo: 'knomit-kb', branch: 'agent/test', context: { kind: 'repo', repo: 'knomit-kb' }, ...over,
});
const lensState = (over: Partial<AppState> = {}): AppState => ({
  ...init, repo: 'core', branch: 'agent/main', context: { kind: 'lens', name: 'eng' }, lens, lensSources: null, ...over,
});

const control = () => screen.queryByTestId('library-factset') as HTMLButtonElement | null;

beforeEach(() => { calls.length = 0; });
afterEach(() => { factsetView.close(); window.history.replaceState(null, '', '/'); });

describe('the FactSet entry control', () => {
  it('relevance: beside the count; opens the repo FactSet of the list search, sort relevance', async () => {
    render(<Library state={repoState({ freeText: 'goroutine', filters: [{ category: 'type', value: 'policy' }] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(calls.some(c => c.fn === 'search')).toBe(true));
    const btn = control()!;
    expect(btn.nextElementSibling?.getAttribute('data-testid')).toBe('library-count');
    expect(btn.disabled).toBe(false);
    fireEvent.click(btn);
    const s = factsetView.get().search!;
    expect(s.target).toEqual({ kind: 'repo', repo: 'knomit-kb', branch: 'agent/test' });
    expect(s.sort).toBe('relevance');
    expect(Object.fromEntries(new URLSearchParams(s.params))).toEqual({ q: 'goroutine', path: 'kb', type: 'policy' });
    // The list was asked with the same search.
    const sc = calls.find(c => c.fn === 'search')!.args;
    expect(sc.slice(2, 4)).toEqual(['goroutine', 'kb']);
    expect(window.location.search).toBe('?factset=1');
  });

  it('a chip turns Path into Recent: the control is there and the FactSet is sort recent with the chip', async () => {
    render(<Library state={repoState({ librarySort: 'path', filters: [{ category: 'domain', value: 'ai' }] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(calls.some(c => c.fn === 'recent')).toBe(true));
    fireEvent.click(control()!);
    const s = factsetView.get().search!;
    expect(s.sort).toBe('recent');
    expect(Object.fromEntries(new URLSearchParams(s.params))).toEqual({ path: 'kb', domain: 'ai' });
  });

  it('is absent in the path tree (not a search)', async () => {
    render(<Library state={repoState({ librarySort: 'path' })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(calls.some(c => c.fn === 'browse')).toBe(true));
    expect(screen.getByTestId('library-count')).toBeTruthy();
    expect(control()).toBeNull();
  });

  it('is disabled in history mode, and a click opens nothing', async () => {
    render(<Library state={repoState({ librarySort: 'recent', asOf: { mode: 'history', commit: 'abc1234' } })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(calls.some(c => c.fn === 'recent')).toBe(true));
    expect(control()!.disabled).toBe(true);
    fireEvent.click(control()!);
    expect(factsetView.get().search).toBeNull();
  });

  it('lens recent, narrowed to one member: the lens route with the list\'s repo= narrowing', async () => {
    render(<Library state={lensState({ librarySort: 'recent', lensSources: ['docs'], filters: [{ category: 'type', value: 'concept' }] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(calls.some(c => c.fn === 'listLensFacts')).toBe(true));
    fireEvent.click(control()!);
    const s = factsetView.get().search!;
    expect(s.target).toEqual({ kind: 'lens', lens: 'eng' });
    expect(s.sort).toBe('recent');
    const p = new URLSearchParams(s.params);
    expect(p.getAll('repo')).toEqual(['docs']);
    expect(p.get('type')).toBe('concept');
    expect(p.get('path')).toBe('kb');
  });

  it('lens relevance: same control, sort relevance', async () => {
    render(<Library state={lensState({ freeText: 'auth' })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(calls.some(c => c.fn === 'lensSearch')).toBe(true));
    fireEvent.click(control()!);
    const s = factsetView.get().search!;
    expect(s.sort).toBe('relevance');
    expect(new URLSearchParams(s.params).get('q')).toBe('auth');
    expect(new URLSearchParams(s.params).has('repo')).toBe(false);
  });
});
