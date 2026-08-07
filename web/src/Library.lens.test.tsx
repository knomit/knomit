import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';
import type { AppState } from './state';
import type { Lens } from './api';
import { repoHue } from './utils';

// Whole-module api mock. In lens context the Library must use the lens
// endpoints; the repo endpoints (browse/recent/search) are mocked too so a
// leak into them is observable (they must NOT be called in lens context).
vi.mock('./api', () => ({
  api: {
    browse: vi.fn().mockResolvedValue({ path: 'kb', children: [] }),
    recent: vi.fn().mockResolvedValue({ facts: [], total: 0 }),
    search: vi.fn().mockResolvedValue({ results: [] }),
    listLensFacts: vi.fn().mockResolvedValue({
      facts: [
        { path: 'kb/ops/rollback.md', title: 'Rollback runbook', type: 'process', committed_at: 100, source: { repo: 'infra', id: 'aaaaaaaaaaaa', branch: 'agent/main' } },
        { path: 'kb://bbbbbbbbbbbb/kb/api/auth.md', title: 'Auth flow', type: 'concept', committed_at: 90, source: { repo: 'docs', id: 'bbbbbbbbbbbb', branch: 'main' } },
      ],
      total: 2,
    }),
    lensSearch: vi.fn().mockResolvedValue([
      { path: 'kb://bbbbbbbbbbbb/kb/api/auth.md', title: 'Auth flow', type: 'concept', source: { repo: 'docs', id: 'bbbbbbbbbbbb', branch: 'main' } },
    ]),
    getLensFact: vi.fn().mockResolvedValue({
      path: 'kb://bbbbbbbbbbbb/kb/api/auth.md', title: 'Auth flow', body: 'b',
      domain: [], entities: [], refs: [], confidence: 1, sources: 1,
      source: { repo: 'docs', id: 'bbbbbbbbbbbb', branch: 'main' },
    }),
  },
}));

const lens: Lens = {
  name: 'eng',
  write: 'core',
  reads: [{ repo: 'core' }, { repo: 'docs' }, { repo: 'infra' }],
};

function lensState(overrides: Partial<AppState> = {}): AppState {
  return {
    ...init,
    repo: 'core',
    branch: 'agent/main',
    headCommit: 'aaaaaaa',
    context: { kind: 'lens', name: 'eng' },
    lens,
    lensSources: null,
    librarySort: 'recent',
    ...overrides,
  };
}

function repoState(overrides: Partial<AppState> = {}): AppState {
  return {
    ...init,
    repo: 'core',
    branch: 'agent/main',
    headCommit: 'aaaaaaa',
    librarySort: 'recent',
    ...overrides,
  };
}

describe('Library — lens read path', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('reads the union via api.listLensFacts, never the repo endpoints', async () => {
    const { api } = await import('./api');
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(2));
    expect(api.listLensFacts).toHaveBeenCalledTimes(1);
    const [name, opts] = (api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(name).toBe('eng');
    expect(opts.repos).toBeUndefined(); // null selection → all mounts → no repos param
    // Repo endpoints must stay untouched in lens context.
    expect(api.recent).not.toHaveBeenCalled();
    expect(api.browse).not.toHaveBeenCalled();
    expect(api.search).not.toHaveBeenCalled();
  });

  it('renders a stable-hued source badge on every union row', async () => {
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(2));
    const badges = screen.getAllByTestId('source-badge');
    expect(badges.length).toBe(2);
    const infra = badges.find(b => b.getAttribute('data-repo') === 'infra')!;
    expect(infra).toBeTruthy();
    expect(infra.textContent).toContain('infra');
    // The per-repo hue is stable across calls and now rides the DOT: the name
    // itself is neutral, matching how the summary panel's Repo rows and the
    // fact band draw a mount. The pill it used to wear is gone.
    expect(infra.style.background).toBe('');
    expect(infra.querySelector('span')!.style.background).toBe(hexToRgb(repoHue('infra')));
  });

  it('strips the kb://<id12>/ qualifier from the displayed path (badge carries the repo)', async () => {
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(2));
    const rows = screen.getAllByTestId('lens-item');
    // The read-mount row keeps the RAW canonical path as its identity…
    const authRow = rows.find(r => r.getAttribute('data-path') === 'kb://bbbbbbbbbbbb/kb/api/auth.md')!;
    expect(authRow).toBeTruthy();
    // …but the displayed breadcrumb shows the stripped path, never the id12.
    const shown = authRow.querySelector('[data-testid="lens-item-path"]')!;
    expect(shown.textContent).toBe('kb/api/auth.md');
    expect(authRow.textContent).not.toContain('bbbbbbbbbbbb');
    expect(authRow.textContent).not.toContain('kb://');
  });

  it('row click navigates with the RAW canonical path and does NOT prefetch/dispatch the source (RightPanel owns the fetch)', async () => {
    const { api } = await import('./api');
    const dispatch = vi.fn();
    const navigate = vi.fn();
    render(<Library state={lensState()} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(2));
    const rows = screen.getAllByTestId('lens-item');
    const authRow = rows.find(r => r.getAttribute('data-path') === 'kb://bbbbbbbbbbbb/kb/api/auth.md')!;
    fireEvent.click(authRow);
    // Navigates with the RAW canonical path so RightPanel can read it through the lens.
    expect(navigate).toHaveBeenCalledWith({ view: 'library', factPath: 'kb://bbbbbbbbbbbb/kb/api/auth.md' });
    // Single-owner: the Library no longer prefetches the fact or dispatches the
    // source — RightPanel's guarded fetch does, avoiding the rapid-re-open race.
    expect(api.getLensFact).not.toHaveBeenCalled();
    expect(dispatch).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: 'SET_FACT_SOURCE' }),
    );
  });

  it('re-sends the repos param and refetches when lensSources changes', async () => {
    const { api } = await import('./api');
    const { rerender } = render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalledTimes(1));
    // Uncheck a mount → explicit subset selection re-fetches with repos=[…].
    rerender(<Library state={lensState({ lensSources: ['core', 'infra'] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalledTimes(2));
    const lastCall = (api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls.at(-1)!;
    expect(lastCall[1].repos).toEqual(['core', 'infra']);
  });

  it('an empty lensSources selection shows an empty state and issues no fetch', async () => {
    const { api } = await import('./api');
    render(<Library state={lensState({ lensSources: [] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.queryAllByTestId('lens-item').length).toBe(0));
    expect(api.listLensFacts).not.toHaveBeenCalled();
    expect(screen.getByText(/no sources selected/i)).toBeTruthy();
  });

  it('free-text search reads via api.lensSearch, not api.search', async () => {
    const { api } = await import('./api');
    render(<Library state={lensState({ freeText: 'auth' })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(1));
    // The union relevance search forwards the current path scope (root 'kb' by
    // default) alongside the query; repos stays undefined for the null selection.
    expect(api.lensSearch).toHaveBeenCalledWith('eng', 'auth', undefined, expect.objectContaining({ path: 'kb' }));
    expect(api.search).not.toHaveBeenCalled();
    // Search rows still carry a source badge.
    expect(screen.getAllByTestId('source-badge').length).toBe(1);
  });
});

describe('Library — sources narrowing', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  // The repo: chip facet is gone, so the sources selection is the ONE thing
  // that narrows the fan-out. These were four tests about intersecting the two.

  it('the null selection sends no repos param — the server fans out to every mount', async () => {
    const { api } = await import('./api');
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalledTimes(1));
    expect((api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls[0][1].repos).toBeUndefined();
  });

  it('a sources selection narrows the fan-out to those mounts, in mount order', async () => {
    const { api } = await import('./api');
    render(<Library state={lensState({ lensSources: ['infra', 'docs'] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalledTimes(1));
    // Mount order (core, docs, infra), not the order they were selected in.
    expect((api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls[0][1].repos).toEqual(['docs', 'infra']);
    // Narrowing WHICH mounts are read must not flip the list into relevance
    // mode — that is ranking, and nothing here asked for a ranking.
    expect(api.lensSearch).not.toHaveBeenCalled();
  });

  it('drops a selected name that is not a real mount rather than 422-ing the server', async () => {
    const { api } = await import('./api');
    render(<Library state={lensState({ lensSources: ['infra', 'ghost'] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalledTimes(1));
    expect((api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls[0][1].repos).toEqual(['infra']);
  });

  it('a selection naming no real mount shows an empty state and issues no fetch', async () => {
    // An empty repos array reads as "all mounts" server-side, so a selection
    // that survives no mount must never become a request. (The distinct
    // "nothing selected at all" case is covered above.)
    const { api } = await import('./api');
    render(<Library state={lensState({ lensSources: ['ghost'] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => screen.getByTestId('left-panel'));
    expect(screen.queryAllByTestId('lens-item').length).toBe(0);
    expect(api.listLensFacts).not.toHaveBeenCalled();
    expect(api.lensSearch).not.toHaveBeenCalled();
    expect(screen.getByText(/no sources match/i)).toBeTruthy();
  });

  it('widening the selection back to null refetches without the narrowing', async () => {
    const { api } = await import('./api');
    const { rerender } = render(<Library state={lensState({ lensSources: ['infra'] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalledTimes(1));
    rerender(<Library state={lensState({ lensSources: null })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalledTimes(2));
    expect((api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls.at(-1)![1].repos).toBeUndefined();
  });

  it('forwards path scope + content filter chips through the lens relevance search', async () => {
    const { api } = await import('./api');
    render(<Library
      state={lensState({ freeText: 'auth', filters: [{ category: 'path', value: 'kb/api' }, { category: 'kind', value: 'policy' }] })}
      dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.lensSearch).toHaveBeenCalled());
    const call = (api.lensSearch as ReturnType<typeof vi.fn>).mock.calls.at(-1)!;
    expect(call[3]).toEqual(expect.objectContaining({ path: 'kb/api', kinds: ['policy'] }));
  });
});

// The mounts picker left the left panel for the top bar. It used to sit here
// under a SOURCES label while the top bar rendered lens.reads.length — the
// TOTAL, whatever was selected — so the same fact appeared twice and only the
// quieter one was true. Its behaviour is now covered by MountsPicker.test.tsx;
// what matters here is that the panel does not render a second one.
describe('Library — the mounts picker is not here', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('renders no sources control in lens context', async () => {
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => screen.getByTestId('left-panel'));
    expect(screen.queryByTestId('mounts-picker')).toBeNull();
    expect(screen.queryByTestId('sources-dropdown')).toBeNull();
  });

  it('renders none in repo context either', async () => {
    render(<Library state={repoState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => screen.getByTestId('left-panel'));
    expect(screen.queryByTestId('mounts-picker')).toBeNull();
  });

  it('still honours a narrowed selection when listing the union', async () => {
    // The control moved; the state it sets still drives this panel's request.
    const { api } = await import('./api');
    render(<Library state={lensState({ lensSources: ['infra'] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalled());
    const opts = vi.mocked(api.listLensFacts).mock.calls[0];
    expect(JSON.stringify(opts)).toContain('infra');
  });
});

// I5: the lens union list is infinite-scroll paged like repo Recent. This
// describe sets a custom listLensFacts implementation, so it runs LAST — the
// per-file mock impl persists across tests (no restoreMocks in config).
describe('Library — lens union paging (I5)', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('first page 50 / total 120 → sentinel loads offset 50 and appends', async () => {
    const { api } = await import('./api');
    const page = (start: number, n: number) =>
      Array.from({ length: n }, (_, i) => ({
        path: `kb/p${start + i}.md`, title: `T${start + i}`, type: 'process',
        committed_at: start + i, source: { repo: 'infra', id: 'aaaaaaaaaaaa', branch: 'agent/main' },
      }));
    (api.listLensFacts as ReturnType<typeof vi.fn>).mockImplementation(async (_lens: string, opts: { offset: number }) => {
      if (opts.offset === 0) return { facts: page(0, 50), total: 120 };
      if (opts.offset === 50) return { facts: page(50, 50), total: 120 };
      return { facts: [], total: 120 };
    });

    // Hold IntersectionObserver callbacks so we can drive the sentinel by hand.
    const observerCallbacks: IntersectionObserverCallback[] = [];
    const origIO = window.IntersectionObserver;
    window.IntersectionObserver = class {
      constructor(cb: IntersectionObserverCallback) { observerCallbacks.push(cb); }
      observe() {} disconnect() {} unobserve() {} takeRecords() { return []; }
      root = null; rootMargin = ''; thresholds = [];
    } as unknown as typeof IntersectionObserver;

    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(50));
    // Sentinel is present in lens context now.
    expect(screen.getByTestId('recent-sentinel')).toBeTruthy();

    // Fire the sentinel intersection → loadMore fetches the next page.
    //
    // Poll for the CONDITION, fire the side effect ONCE, then poll for the
    // result. An earlier version fired the intersection callback INSIDE the
    // waitFor body — and waitFor re-runs its callback on every poll tick and on
    // every DOM mutation, so the sentinel fired an unbounded number of times.
    // It was safe only by accident (lensLoadingRef swallows the repeats and the
    // assertion is `toContain`), and it hid the real defect rather than testing
    // around it: Library used to re-create its IntersectionObserver on every
    // `loadMore` identity change, so firing `.at(-1)` could invoke a stale
    // generation whose guard read `lensRows.length >= total` as `0 >= 0` and
    // silently no-op'd (~1 in 15 full-suite runs). The observer is now created
    // once per paged list and calls loadMore through a ref, so there is exactly
    // one live callback and one fire is enough.
    await waitFor(() => expect(observerCallbacks.length).toBeGreaterThan(0));
    act(() => {
      observerCallbacks[observerCallbacks.length - 1](
        [{ isIntersecting: true } as IntersectionObserverEntry],
        {} as IntersectionObserver,
      );
    });
    await waitFor(() =>
      expect((api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls.map(c => c[1].offset)).toContain(50));

    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(100));
    const offsets = (api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls.map(c => c[1].offset);
    expect(offsets).toContain(0);
    expect(offsets).toContain(50);

    window.IntersectionObserver = origIO;
  });

  it('a scope change mid-loadMore drops the stale-scope page instead of appending it', async () => {
    const { api } = await import('./api');
    const mk = (prefix: string, start: number, n: number, repo: string) =>
      Array.from({ length: n }, (_, i) => ({
        path: `kb/${prefix}${start + i}.md`, title: `${prefix}${start + i}`, type: 'process',
        committed_at: start + i, source: { repo, id: 'aaaaaaaaaaaa', branch: 'agent/main' },
      }));

    // A deferred we release by hand models a loadMore fetch still in flight when
    // the scope narrows.
    let releaseStale!: (v: unknown) => void;
    const stalePage = new Promise<unknown>(res => { releaseStale = res; });

    (api.listLensFacts as ReturnType<typeof vi.fn>).mockImplementation(
      async (_lens: string, opts: { offset: number; repos?: string[] }) => {
        // ALL-mounts scope page 1 (repos omitted): 50 rows, total pages.
        if (opts.offset === 0 && opts.repos === undefined) return { facts: mk('A', 0, 50, 'infra'), total: 120 };
        // The loadMore fetch for the ALL scope — stays pending until released.
        if (opts.offset === 50) return stalePage;
        // NARROWED scope page 1 (repos=['infra']): 2 rows, fully loaded.
        if (opts.offset === 0) return { facts: mk('B', 0, 2, 'infra'), total: 2 };
        return { facts: [], total: 0 };
      });

    const observerCallbacks: IntersectionObserverCallback[] = [];
    const origIO = window.IntersectionObserver;
    window.IntersectionObserver = class {
      constructor(cb: IntersectionObserverCallback) { observerCallbacks.push(cb); }
      observe() {} disconnect() {} unobserve() {} takeRecords() { return []; }
      root = null; rootMargin = ''; thresholds = [];
    } as unknown as typeof IntersectionObserver;

    const { rerender } = render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(50));

    // Fire the sentinel: loadMore snapshots the current generation and awaits the
    // (still-pending) offset-50 fetch. Wait for the observer, fire once, then
    // wait for the fetch — no side effect inside a waitFor body (see above).
    await waitFor(() => expect(observerCallbacks.length).toBeGreaterThan(0));
    act(() => {
      observerCallbacks[observerCallbacks.length - 1](
        [{ isIntersecting: true } as IntersectionObserverEntry], {} as IntersectionObserver,
      );
    });
    await waitFor(() =>
      expect((api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls.map(c => c[1].offset)).toContain(50));

    // Narrow the scope while offset-50 is still pending. The union effect bumps
    // the generation and resets the list to the narrowed page 1 (B0, B1).
    rerender(<Library state={lensState({ lensSources: ['infra'] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(2));
    expect(screen.getByText('B0')).toBeTruthy();

    // The stale offset-50 response now arrives. It belonged to the old (all-mounts)
    // scope and MUST be dropped, not appended onto the narrowed list.
    releaseStale({ facts: mk('A', 50, 50, 'docs'), total: 120 });
    await new Promise(r => setTimeout(r));

    const titles = screen.getAllByTestId('lens-item').map(e => e.textContent || '');
    expect(titles.some(t => t.includes('A5'))).toBe(false); // no stale-scope rows leaked
    expect(screen.getAllByTestId('lens-item').length).toBe(2);

    window.IntersectionObserver = origIO;
  });
});

// A filter pick is one user action that must land the reader ON a result, not
// merely narrow a list behind a summary panel. The repo list has always
// auto-selected its first row (api.recent and api.search both AMEND_NAV); the
// lens union did not, so picking a domain in a lens filtered the left panel and
// left the right panel sitting on the folder dashboard.
describe('Library — lens union auto-select', () => {
  const AUTH = 'kb://bbbbbbbbbbbb/kb/api/auth.md';
  const ROLLBACK = 'kb/ops/rollback.md';

  // Re-arm the union mocks rather than inheriting the file-scope ones:
  // clearAllMocks drops calls, not implementations, so a preceding test that
  // swapped in a 50-row pager would otherwise decide what these rows are.
  beforeEach(async () => {
    vi.clearAllMocks();
    const { api } = await import('./api');
    (api.listLensFacts as ReturnType<typeof vi.fn>).mockResolvedValue({
      facts: [
        { path: ROLLBACK, title: 'Rollback runbook', type: 'process', committed_at: 100,
          source: { repo: 'infra', id: 'aaaaaaaaaaaa', branch: 'agent/main' } },
        { path: AUTH, title: 'Auth flow', type: 'concept', committed_at: 90,
          source: { repo: 'docs', id: 'bbbbbbbbbbbb', branch: 'main' } },
      ],
      total: 2,
    });
    (api.lensSearch as ReturnType<typeof vi.fn>).mockResolvedValue([
      { path: AUTH, title: 'Auth flow', type: 'concept',
        source: { repo: 'docs', id: 'bbbbbbbbbbbb', branch: 'main' } },
    ]);
  });
  const amends = (dispatch: ReturnType<typeof vi.fn>) =>
    dispatch.mock.calls.map(c => c[0]).filter(a => a.type === 'AMEND_NAV');

  it('opens the first union row when a CHIP narrows the list', async () => {
    // A chip no longer routes to the relevance endpoint — it is a filter, so
    // the sort stays put and the paged facts union answers it with the chip
    // forwarded. What must not change is the auto-open: a narrowed list still
    // lands the reader on its first row rather than on the dashboard.
    const dispatch = vi.fn();
    render(<Library state={lensState({ filters: [{ category: 'domain', value: 'ai' }] })}
      dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(2));
    await waitFor(() => expect(amends(dispatch)).toEqual([{ type: 'AMEND_NAV', factPath: ROLLBACK }]));
  });

  it('opens the first union row when FREE TEXT narrows the list', async () => {
    const dispatch = vi.fn();
    render(<Library state={lensState({ freeText: 'auth' })} dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(1));
    await waitFor(() => expect(amends(dispatch)).toEqual([{ type: 'AMEND_NAV', factPath: AUTH }]));
  });

  it('opens the first union row in recency mode too', async () => {
    const dispatch = vi.fn();
    render(<Library state={lensState()} dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(2));
    await waitFor(() => expect(amends(dispatch)).toEqual([{ type: 'AMEND_NAV', factPath: ROLLBACK }]));
  });

  it('keeps the open fact selected when it survives the refetch', async () => {
    // Re-selecting row 0 on every refetch would yank the reader off the fact
    // they are reading whenever an unrelated chip changes the list.
    const dispatch = vi.fn();
    render(<Library state={lensState({ factPath: AUTH })} dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(2));
    expect(amends(dispatch)).toEqual([]);
  });

  it('replaces an open fact that the new filter excluded', async () => {
    const dispatch = vi.fn();
    render(<Library state={lensState({ factPath: 'kb/gone/elsewhere.md' })}
      dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(2));
    await waitFor(() => expect(amends(dispatch)).toEqual([{ type: 'AMEND_NAV', factPath: ROLLBACK }]));
  });

  it('opens nothing when the union comes back empty', async () => {
    const { api } = await import('./api');
    (api.listLensFacts as ReturnType<typeof vi.fn>).mockResolvedValueOnce({ facts: [], total: 0 });
    const dispatch = vi.fn();
    render(<Library state={lensState()} dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalled());
    await new Promise(r => setTimeout(r, 10));
    expect(amends(dispatch)).toEqual([]);
  });
});

// jsdom serializes an element's inline `color` as `rgb(r, g, b)`. Convert a
// #rrggbb hue for comparison.
function hexToRgb(hex: string): string {
  const r = parseInt(hex.slice(1, 3), 16);
  const g = parseInt(hex.slice(3, 5), 16);
  const b = parseInt(hex.slice(5, 7), 16);
  return `rgb(${r}, ${g}, ${b})`;
}

// The lens union list ignored content chips entirely. api.listLensFacts took
// only path/query/limit/offset/repos, so a domain chip reached the effect's dep
// array (filtersKey is in it), triggered a refetch, and sent the identical
// unfiltered request — the reader clicked "ai · 666" and got the whole corpus
// back with a chip sitting above it.
//
// It was a workaround that outlived its reason: the lens FACTS handler could
// not take filters when this was written, so filter-bearing reads were routed
// through /search instead. The handler gained them; the client never followed.
describe('Library — content chips reach the union list', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('forwards a domain chip to listLensFacts', async () => {
    const { api } = await import('./api');
    render(<Library state={lensState({ filters: [{ category: 'domain', value: 'ai' }] })}
      dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalled());
    const [, opts] = (api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(opts.domains).toEqual(['ai']);
  });

  it('forwards type and entity chips too', async () => {
    const { api } = await import('./api');
    render(<Library state={lensState({ filters: [
      { category: 'type', value: 'synthesis' },
      { category: 'entity', value: 'Anthropic' },
    ] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.listLensFacts).toHaveBeenCalled());
    const [, opts] = (api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls[0];
    expect(opts.types).toEqual(['synthesis']);
    expect(opts.entities).toEqual(['Anthropic']);
  });

  it('keeps the chip when paging — page 2 of a filtered list is still filtered', async () => {
    // Dropping it on loadMore would silently append unfiltered rows to a
    // filtered list, which reads as the filter breaking halfway down.
    const { api } = await import('./api');
    vi.mocked(api.listLensFacts).mockResolvedValue({
      facts: Array.from({ length: 50 }, (_, i) => ({
        path: `kb/x/${i}.md`, title: `t${i}`, type: 'observation', committed_at: 100 - i,
        source: { repo: 'infra', id: 'aaaaaaaaaaaa', branch: 'agent/main' },
      })),
      total: 500,
    });
    render(<Library state={lensState({ filters: [{ category: 'domain', value: 'ai' }] })}
      dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(50));
    await act(async () => {
      const sentinel = screen.getByTestId('recent-sentinel');
      sentinel.scrollIntoView?.();
      window.dispatchEvent(new Event('scroll'));
    });
    const calls = (api.listLensFacts as ReturnType<typeof vi.fn>).mock.calls;
    for (const [, opts] of calls) expect(opts.domains).toEqual(['ai']);
  });
});
