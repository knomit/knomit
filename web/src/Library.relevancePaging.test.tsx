import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, act } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';
import type { AppState } from './state';
import type { Lens } from './api';

// The relevance list scrolls like Recent (user report 2026-11-02: the "all"
// lens searched "agentic" showed 50 rows and would not scroll, while its
// FactSet held 500). The /search routes rank ONE list and page it by offset;
// the list asks for page 2 when the sentinel intersects, and stops at the
// first short page, repo and lens alike.
vi.mock('./api', () => ({
  api: {
    browse: vi.fn().mockResolvedValue({ path: 'kb', children: [] }),
    recent: vi.fn().mockResolvedValue({ facts: [], total: 0 }),
    search: vi.fn(),
    listLensFacts: vi.fn().mockResolvedValue({ facts: [], total: 0 }),
    lensSearch: vi.fn(),
  },
}));

const lens: Lens = {
  name: 'eng',
  write: { uid: 'uid-core', name: 'core' },
  reads: [{ uid: 'uid-core', name: 'core' }, { uid: 'uid-docs', name: 'docs' }],
};

const base: AppState = { ...init, repo: 'core', branch: 'agent/main', headCommit: 'aaaaaaa', librarySort: 'recent', freeText: 'agentic' };
const lensState: AppState = { ...base, context: { kind: 'lens', name: 'eng' }, lens, lensSources: null };

// A ranked list of n rows; a page is its slice [offset, offset+limit).
function ranked(n: number) {
  return Array.from({ length: n }, (_, i) => ({
    path: `kb/r/f${String(i).padStart(3, '0')}.md`, title: `R${i}`, type: 'observation',
    source: { repo: 'core', id: 'aaaaaaaaaaaa', branch: 'agent/main' },
  }));
}

// Hold the IntersectionObserver callbacks so the test drives the sentinel:
// jsdom never scrolls.
let observers: IntersectionObserverCallback[] = [];
const origIO = window.IntersectionObserver;
beforeEach(() => {
  vi.clearAllMocks();
  observers = [];
  window.IntersectionObserver = class {
    constructor(cb: IntersectionObserverCallback) { observers.push(cb); }
    observe() {}
    disconnect() {}
    unobserve() {}
    takeRecords() { return []; }
    root = null; rootMargin = ''; thresholds = [];
  } as unknown as typeof IntersectionObserver;
});
afterEach(() => { window.IntersectionObserver = origIO; });

function intersect() {
  expect(observers.length).toBeGreaterThan(0);
  act(() => {
    observers[observers.length - 1]([{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver);
  });
}

// Sabotage: leave the relevance list unpaged (paged false for relevance, or
// loadMore returning early on it) and page 2 is never asked for; drop the
// short-page terminator and a third request goes out past the end.
describe('Library — relevance list infinite scroll', () => {
  it('repo: loads page 2 at offset 50 on scroll, then stops at the short page', async () => {
    const { api } = await import('./api');
    const all = ranked(70);
    const search = api.search as ReturnType<typeof vi.fn>;
    search.mockImplementation(async (_r: string, _b: string, _q: string, _p: string, _c: number, _o: unknown, page?: { limit?: number; offset?: number }) => {
      const off = page?.offset ?? 0, lim = page?.limit ?? 50;
      return { results: all.slice(off, off + lim) };
    });
    render(<Library state={base} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(50));
    expect(search.mock.calls.map(c => c[6])).toEqual([{ limit: 50, offset: 0 }]);

    intersect();
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(70));
    expect(search.mock.calls.map(c => c[6])).toEqual([{ limit: 50, offset: 0 }, { limit: 50, offset: 50 }]);
    const titles = screen.getAllByTestId('dir-entry').map(e => e.textContent ?? '');
    expect(titles[50]).toContain('R50');

    // Page 2 was short: the end of the list. Another tick asks for nothing.
    intersect();
    await Promise.resolve();
    expect(search).toHaveBeenCalledTimes(2);
  });

  it('lens: loads page 2 at offset 50 on scroll, then stops at the short page', async () => {
    const { api } = await import('./api');
    const all = ranked(60);
    const lensSearch = api.lensSearch as ReturnType<typeof vi.fn>;
    lensSearch.mockImplementation(async (_l: string, _q: string, _r: unknown, _o: unknown, page?: { limit?: number; offset?: number }) => {
      const off = page?.offset ?? 0, lim = page?.limit ?? 50;
      return all.slice(off, off + lim);
    });
    render(<Library state={lensState} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(50));
    expect(lensSearch.mock.calls.map(c => c[4])).toEqual([{ limit: 50, offset: 0 }]);

    intersect();
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(60));
    expect(lensSearch.mock.calls.map(c => c[4])).toEqual([{ limit: 50, offset: 0 }, { limit: 50, offset: 50 }]);
    // The same scope on both pages: query, repos (all), path.
    expect(lensSearch.mock.calls[1].slice(0, 4)).toEqual(lensSearch.mock.calls[0].slice(0, 4));

    intersect();
    await Promise.resolve();
    expect(lensSearch).toHaveBeenCalledTimes(2);
    expect(api.search).not.toHaveBeenCalled();
  });

  it('a list that fits one page never asks for a second', async () => {
    const { api } = await import('./api');
    const search = api.search as ReturnType<typeof vi.fn>;
    search.mockResolvedValue({ results: ranked(12) });
    render(<Library state={base} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(12));
    if (observers.length) intersect();
    await Promise.resolve();
    expect(search).toHaveBeenCalledTimes(1);
  });
});
