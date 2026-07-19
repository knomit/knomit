import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
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
    // The badge colors are the deterministic per-repo hue (stable across calls).
    expect(infra.style.color).toBe(hexToRgb(repoHue('infra')));
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

  it('row click opens the fact via getLensFact with the RAW path and stores its source', async () => {
    const { api } = await import('./api');
    const dispatch = vi.fn();
    const navigate = vi.fn();
    render(<Library state={lensState()} dispatch={dispatch} navigate={navigate} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-item').length).toBe(2));
    const rows = screen.getAllByTestId('lens-item');
    const authRow = rows.find(r => r.getAttribute('data-path') === 'kb://bbbbbbbbbbbb/kb/api/auth.md')!;
    fireEvent.click(authRow);
    // Opens the fact through the lens with the RAW canonical path.
    expect(api.getLensFact).toHaveBeenCalledWith('eng', 'kb://bbbbbbbbbbbb/kb/api/auth.md');
    // Mirrors the repo open flow so RightPanel renders the fact.
    expect(navigate).toHaveBeenCalledWith({ view: 'library', factPath: 'kb://bbbbbbbbbbbb/kb/api/auth.md' });
    // Stores the source mount once getLensFact resolves.
    await waitFor(() => expect(dispatch).toHaveBeenCalledWith({
      type: 'SET_FACT_SOURCE',
      source: { repo: 'docs', id: 'bbbbbbbbbbbb', branch: 'main' },
    }));
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
    expect(api.lensSearch).toHaveBeenCalledWith('eng', 'auth', undefined);
    expect(api.search).not.toHaveBeenCalled();
    // Search rows still carry a source badge.
    expect(screen.getAllByTestId('source-badge').length).toBe(1);
  });
});

describe('Library — sources dropdown', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('renders only in lens context', async () => {
    const { rerender } = render(<Library state={repoState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => screen.getByTestId('left-panel'));
    expect(screen.queryByTestId('sources-dropdown')).toBeNull();
    rerender(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getByTestId('sources-dropdown')).toBeTruthy());
  });

  it('closed control labels "All mounts · N" for the null (all) selection', async () => {
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => screen.getByTestId('sources-dropdown'));
    expect(screen.getByTestId('sources-label').textContent).toMatch(/All mounts.*3/);
  });

  it('closed control labels "<n> of N mounts" when filtered', async () => {
    render(<Library state={lensState({ lensSources: ['infra'] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => screen.getByTestId('sources-dropdown'));
    expect(screen.getByTestId('sources-label').textContent).toMatch(/1 of 3 mounts/);
  });

  it('lists one checklist row per lens.reads mount and toggling dispatches SET_LENS_SOURCES', async () => {
    const dispatch = vi.fn();
    render(<Library state={lensState()} dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => screen.getByTestId('sources-dropdown'));
    fireEvent.click(screen.getByTestId('sources-dropdown'));
    const options = screen.getAllByTestId('source-option');
    expect(options.map(o => o.getAttribute('data-repo'))).toEqual(['core', 'docs', 'infra']);
    // All-on to start; unchecking 'docs' yields the remaining mounts in order.
    const docs = options.find(o => o.getAttribute('data-repo') === 'docs')!;
    fireEvent.click(docs);
    expect(dispatch).toHaveBeenCalledWith({ type: 'SET_LENS_SOURCES', repos: ['core', 'infra'] });
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
