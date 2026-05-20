import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';
import type { AppState } from './state';

vi.mock('./api', () => ({
  api: {
    browse: vi.fn().mockResolvedValue({
      path: 'kb',
      children: [
        { name: 'sub', is_dir: true },
        { name: 'fact.md', is_dir: false, title: 'A Fact', type: 'observation', fullPath: 'kb/fact.md' },
      ],
    }),
    recent: vi.fn().mockResolvedValue({ facts: [], total: 0 }),
    search: vi.fn().mockResolvedValue({ results: [] }),
  },
}));

function setup(overrides: Partial<AppState> = {}) {
  const state: AppState = {
    ...init,
    repo: 'knomit',
    branch: 'machine/test',
    headCommit: 'aaaaaaa',
    ...overrides,
  };
  return render(<Library state={state} dispatch={vi.fn()} navigate={vi.fn()} />);
}

describe('Library — Path sort', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('renders dir-entry rows from api.browse', async () => {
    setup({ librarySort: 'path' });
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBeGreaterThan(0));
    const rows = screen.getAllByTestId('dir-entry');
    expect(rows.length).toBe(2);
  });

  it('exposes data-sort="path" on the container', async () => {
    setup({ librarySort: 'path' });
    await waitFor(() => screen.getByTestId('left-panel'));
    expect(screen.getByTestId('left-panel').getAttribute('data-sort')).toBe('path');
  });
});

describe('Library — Recent sort', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('renders chrono-item rows from api.recent', async () => {
    const { api } = await import('./api');
    (api.recent as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      facts: [
        { path: 'kb/a.md', title: 'A', committed_at: 1, type: 'observation' },
        { path: 'kb/b.md', title: 'B', committed_at: 2, type: 'observation' },
      ],
      total: 2,
    });
    setup({ librarySort: 'recent' });
    await waitFor(() => expect(screen.getAllByTestId('chrono-item').length).toBe(2));
  });

  it('exposes data-sort="recent" on the container', async () => {
    setup({ librarySort: 'recent' });
    await waitFor(() => screen.getByTestId('left-panel'));
    expect(screen.getByTestId('left-panel').getAttribute('data-sort')).toBe('recent');
  });
});

describe('Library — Recent sort infinite scroll', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('calls api.recent with offset=facts.length when the sentinel intersects', async () => {
    const { api } = await import('./api');
    const page1 = Array.from({ length: 50 }, (_, i) => ({
      path: `kb/p${i}.md`, title: `T${i}`, committed_at: i, type: 'observation',
    }));
    const page2 = Array.from({ length: 50 }, (_, i) => ({
      path: `kb/p${i + 50}.md`, title: `T${i + 50}`, committed_at: i + 50, type: 'observation',
    }));
    (api.recent as ReturnType<typeof vi.fn>).mockImplementation(async (
      _repo: string, _branch: string, _path: string, _q: string,
      _limit: number, offset: number,
    ) => {
      if (offset === 0) return { facts: page1, total: 100 };
      if (offset === 50) return { facts: page2, total: 100 };
      return { facts: [], total: 100 };
    });

    // Hold IntersectionObserver callbacks so we can drive them by hand —
    // jsdom doesn't actually scroll, so the observer never naturally fires.
    const observerCallbacks: IntersectionObserverCallback[] = [];
    const origIO = window.IntersectionObserver;
    // @ts-expect-error — minimal stub for the test
    window.IntersectionObserver = class {
      constructor(cb: IntersectionObserverCallback) { observerCallbacks.push(cb); }
      observe() {}
      disconnect() {}
      unobserve() {}
      takeRecords() { return []; }
      root = null; rootMargin = ''; thresholds = [];
    };

    setup({ librarySort: 'recent' });
    await waitFor(() => expect(screen.getAllByTestId('chrono-item').length).toBe(50));

    // Trigger the sentinel intersection.
    expect(observerCallbacks.length).toBeGreaterThan(0);
    observerCallbacks[observerCallbacks.length - 1](
      [{ isIntersecting: true } as IntersectionObserverEntry],
      {} as IntersectionObserver,
    );

    await waitFor(() => expect(screen.getAllByTestId('chrono-item').length).toBe(100));
    // api.recent was called twice: offset 0 then offset 50.
    const offsets = (api.recent as ReturnType<typeof vi.fn>).mock.calls.map(c => c[5]);
    expect(offsets).toContain(0);
    expect(offsets).toContain(50);

    window.IntersectionObserver = origIO;
  });

  it('does not load more once facts.length reaches total', async () => {
    const { api } = await import('./api');
    (api.recent as ReturnType<typeof vi.fn>).mockResolvedValue({
      facts: Array.from({ length: 30 }, (_, i) => ({
        path: `kb/p${i}.md`, title: `T${i}`, committed_at: i, type: 'observation',
      })),
      total: 30,
    });

    const observerCallbacks: IntersectionObserverCallback[] = [];
    const origIO = window.IntersectionObserver;
    // @ts-expect-error — minimal stub for the test
    window.IntersectionObserver = class {
      constructor(cb: IntersectionObserverCallback) { observerCallbacks.push(cb); }
      observe() {}
      disconnect() {}
      unobserve() {}
      takeRecords() { return []; }
      root = null; rootMargin = ''; thresholds = [];
    };

    setup({ librarySort: 'recent' });
    await waitFor(() => expect(screen.getAllByTestId('chrono-item').length).toBe(30));

    const initialCallCount = (api.recent as ReturnType<typeof vi.fn>).mock.calls.length;
    observerCallbacks[observerCallbacks.length - 1](
      [{ isIntersecting: true } as IntersectionObserverEntry],
      {} as IntersectionObserver,
    );
    // Give it a microtask flush.
    await Promise.resolve();
    expect((api.recent as ReturnType<typeof vi.fn>).mock.calls.length).toBe(initialCallCount);

    window.IntersectionObserver = origIO;
  });
});

describe('Library — Recent sort keyboard navigation', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('ArrowDown in Recent mode navigates between facts', async () => {
    const { api } = await import('./api');
    (api.recent as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      facts: [
        { path: 'kb/a.md', title: 'A', committed_at: 1, type: 'observation' },
        { path: 'kb/b.md', title: 'B', committed_at: 2, type: 'observation' },
      ],
      total: 2,
    });
    const navigate = vi.fn();
    render(<Library state={{
      ...init,
      repo: 'knomit',
      branch: 'machine/test',
      headCommit: 'aaaaaaa',
      librarySort: 'recent',
    }} dispatch={vi.fn()} navigate={navigate} />);
    await waitFor(() => expect(screen.getAllByTestId('chrono-item').length).toBe(2));
    fireEvent.keyDown(window, { key: 'ArrowDown' });
    expect(navigate).toHaveBeenCalledWith({ view: 'library', factPath: 'kb/a.md' });
  });
});

describe('Library — Relevance sort', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('renders results from api.search when freeText is set', async () => {
    const { api } = await import('./api');
    (api.search as ReturnType<typeof vi.fn>).mockResolvedValueOnce({
      results: [
        { path: 'kb/x.md', title: 'X result', type: 'observation' },
      ],
    });
    setup({ librarySort: 'recent', freeText: 'foo' });
    // freeText makes effectiveSort='relevance' regardless of stored value
    await waitFor(() => expect(screen.getByTestId('left-panel').getAttribute('data-sort')).toBe('relevance'));
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBeGreaterThan(0));
  });
});
