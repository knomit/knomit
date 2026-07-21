import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';
import type { AppState } from './state';
import type { Lens } from './api';

// Whole-module api mock. In lens+path mode the Library must read the unified
// tree via lensBrowse; every other list endpoint is mocked too so a leak into
// them is observable.
vi.mock('./api', () => ({
  api: {
    browse: vi.fn().mockResolvedValue({ path: 'kb', children: [{ name: 'decisions', is_dir: true }] }),
    recent: vi.fn().mockResolvedValue({ facts: [], total: 0 }),
    search: vi.fn().mockResolvedValue({ results: [] }),
    listLensFacts: vi.fn().mockResolvedValue({ facts: [], total: 0 }),
    lensSearch: vi.fn().mockResolvedValue([]),
    lensBrowse: vi.fn().mockResolvedValue({
      path: 'kb',
      children: [
        { name: 'decisions', is_dir: true },
        { name: 'aaa.md', is_dir: false, type: 'decision', title: 'Write fact', path: 'kb/aaa.md', source: { repo: 'core', id: 'aaaaaaaaaaaa' } },
        { name: 'bbb.md', is_dir: false, type: 'gotcha', title: 'Docs fact', path: 'kb://bbbbbbbbbbbb/kb/bbb.md', source: { repo: 'docs', id: 'bbbbbbbbbbbb' } },
      ],
    }),
  },
}));

const lens: Lens = {
  name: 'eng',
  write: 'core',
  reads: [{ repo: 'core' }, { repo: 'docs' }],
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
    librarySort: 'path',
    ...overrides,
  };
}

function repoState(overrides: Partial<AppState> = {}): AppState {
  return {
    ...init,
    repo: 'core',
    branch: 'agent/main',
    headCommit: 'aaaaaaa',
    librarySort: 'path',
    ...overrides,
  };
}

describe('Library — lens tree browse (Path sort)', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('reads the unified tree via api.lensBrowse, not listLensFacts/browse', async () => {
    const { api } = await import('./api');
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-tree-entry').length).toBe(3));
    expect(api.lensBrowse).toHaveBeenCalledTimes(1);
    // Root path 'kb', the state's ontologyRoot, and no repos param (null selection).
    expect(api.lensBrowse).toHaveBeenCalledWith('eng', 'kb', 'kb', undefined);
    expect(api.listLensFacts).not.toHaveBeenCalled();
    expect(api.browse).not.toHaveBeenCalled();
    expect(api.search).not.toHaveBeenCalled();
    expect(api.lensSearch).not.toHaveBeenCalled();
  });

  it('renders merged dirs plain and fact leaves with source badges', async () => {
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-tree-entry').length).toBe(3));
    const rows = screen.getAllByTestId('lens-tree-entry');
    // Server order preserved: merged dir first, then leaves.
    expect(rows.map(r => r.getAttribute('data-name'))).toEqual(['decisions', 'aaa.md', 'bbb.md']);
    expect(rows[0].getAttribute('data-isdir')).toBe('true');
    // Leaves carry a badge in the source repo's hue; the dir row has none.
    const badges = screen.getAllByTestId('source-badge');
    expect(badges.map(b => b.getAttribute('data-repo'))).toEqual(['core', 'docs']);
    expect(rows[0].querySelector('[data-testid="source-badge"]')).toBeNull();
    // Leaf rows show the enriched title.
    expect(rows[1].textContent).toContain('Write fact');
    expect(rows[2].textContent).toContain('Docs fact');
  });

  it('a directory row navigates one level deeper (path chip, matching the repo tree)', async () => {
    const dispatch = vi.fn();
    render(<Library state={lensState()} dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-tree-entry').length).toBe(3));
    const dir = screen.getAllByTestId('lens-tree-entry')[0];
    fireEvent.click(dir);
    expect(dispatch).toHaveBeenCalledWith({ type: 'ADD_FILTER', chip: { category: 'path', value: 'kb/decisions' } });
  });

  it('a fact leaf opens with the RAW canonical qualified path', async () => {
    const navigate = vi.fn();
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={navigate} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-tree-entry').length).toBe(3));
    const leaf = screen.getAllByTestId('lens-tree-entry')
      .find(r => r.getAttribute('data-path') === 'kb://bbbbbbbbbbbb/kb/bbb.md')!;
    fireEvent.click(leaf);
    expect(navigate).toHaveBeenCalledWith({ view: 'library', factPath: 'kb://bbbbbbbbbbbb/kb/bbb.md' });
  });

  it('forwards the repos narrowing to lensBrowse', async () => {
    const { api } = await import('./api');
    render(<Library state={lensState({ lensSources: ['core'] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.lensBrowse).toHaveBeenCalledTimes(1));
    expect(api.lensBrowse).toHaveBeenCalledWith('eng', 'kb', 'kb', ['core']);
  });

  it('an empty scope shows the empty state and issues no fetch', async () => {
    const { api } = await import('./api');
    render(<Library state={lensState({ lensSources: [] })} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => screen.getByTestId('left-panel'));
    expect(screen.queryAllByTestId('lens-tree-entry').length).toBe(0);
    expect(api.lensBrowse).not.toHaveBeenCalled();
    expect(screen.getByText(/no sources selected/i)).toBeTruthy();
  });

  it('lens+path is not infinite-scroll paged (no sentinel)', async () => {
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-tree-entry').length).toBe(3));
    expect(screen.queryByTestId('recent-sentinel')).toBeNull();
  });

  it('repo-context Path browse still uses api.browse, never lensBrowse', async () => {
    const { api } = await import('./api');
    render(<Library state={repoState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(api.browse).toHaveBeenCalledTimes(1));
    expect(api.browse).toHaveBeenCalledWith('core', 'agent/main', 'kb', 'kb');
    expect(api.lensBrowse).not.toHaveBeenCalled();
    expect(screen.getAllByTestId('dir-entry').length).toBe(1);
  });
});
