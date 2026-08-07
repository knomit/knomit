import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';
import type { AppState } from './state';
import type { Lens } from './api';
import { repoHue } from './utils';

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

  // NAVIGATE, not ADD_FILTER{path}: entering a directory is one action whatever
  // triggers it, so a click and the Enter key cannot drift apart (they had).
  it('a directory row navigates one level deeper, matching the repo tree', async () => {
    const dispatch = vi.fn();
    render(<Library state={lensState()} dispatch={dispatch} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-tree-entry').length).toBe(3));
    const dir = screen.getAllByTestId('lens-tree-entry')[0];
    fireEvent.click(dir);
    expect(dispatch).toHaveBeenCalledWith({ type: 'NAVIGATE', path: 'kb/decisions' });
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

  // The tree read as a GRID: a hairline under every row turned a list of
  // sixteen topics into sixteen boxes. The panel should read as one surface.
  it('draws no separator under a tree row', async () => {
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-tree-entry').length).toBe(3));
    for (const row of screen.getAllByTestId('lens-tree-entry')) {
      expect(row.style.borderBottom).toBe('');
    }
  });

  it('lights the row under the cursor, since the separator no longer bounds it', async () => {
    // Without the hairline AND without hover there is nothing to tell you which
    // row you are about to open — the rows would be a column of loose text.
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('lens-tree-entry').length).toBe(3));
    const row = screen.getAllByTestId('lens-tree-entry')[0];
    expect(row.style.background).toBe('transparent');
    fireEvent.mouseEnter(row);
    expect(row.style.background).not.toBe('transparent');
    fireEvent.mouseLeave(row);
    expect(row.style.background).toBe('transparent');
  });

  it('leaves the selected row alone on hover', async () => {
    // Selection outranks pointing at something: repainting the selected row on
    // hover would make it look unselected the moment you reached for it.
    render(<Library state={lensState({ factPath: 'kb/aaa.md' })} dispatch={vi.fn()} navigate={vi.fn()} />);
    // Wait for the SELECTION, not merely for the rows: selectedIdx is applied by
    // an effect that runs after the fetch resolves, so asserting on row count
    // alone can catch the row one render before it is selected — and an
    // unselected row is exactly the one that DOES take the hover.
    const row = () => screen.getAllByTestId('lens-tree-entry')
      .find(r => r.getAttribute('data-path') === 'kb/aaa.md')!;
    await waitFor(() => expect(row().style.background).not.toBe('transparent'));
    const selected = row();
    const before = selected.style.background;
    fireEvent.mouseEnter(selected);
    expect(selected.style.background).toBe(before);
  });

  it('gives a tree leaf its title on one line and its mount on the next', () => {
    // On a narrow panel the mount badge sat BESIDE the title and refused to
    // shrink, so the title took every character of the truncation: rows read
    // "Agent failure…" next to a full-width "agentic-engineering". The flat
    // lens list already stacks them, and it stays readable.
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    return waitFor(() => {
      const badge = screen.getAllByTestId('source-badge')[0];
      // The badge now shares a meta ROW with the path, the way the flat union
      // row does, so the column is that row's parent rather than the badge's.
      const stack = badge.closest('[style*="column"]') as HTMLElement;
      expect(stack.style.flexDirection).toBe('column');
      expect(stack.textContent).toContain(badge.textContent);
      // The title is the thing above: the badge is not in the first line.
      expect(stack.firstElementChild!.contains(badge)).toBe(false);
    });
  });

  it('gives that second line the same two items the flat union row has', () => {
    // The two lens views disagreed here. Inside a directory the tree row showed
    // the mount alone while Recent showed the mount AND the breadcrumb again on
    // every row — same location, same list, opposite failures. Both now show
    // the mount and the part of the path the header is not already showing.
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    return waitFor(() => {
      const badge = screen.getAllByTestId('source-badge')[0];
      const line = badge.parentElement!;
      const path = line.querySelector('[data-testid="entry-path"]');
      expect(path).toBeTruthy();
      expect(path!.textContent).toBeTruthy();
    });
  });

  it('draws the mount the way the rest of the app now does — no pill', () => {
    // A dot and a plain mono name, as the summary panel's Repo rows and the
    // fact band use. The bordered, filled pill was the last one left.
    render(<Library state={lensState()} dispatch={vi.fn()} navigate={vi.fn()} />);
    return waitFor(() => {
      const badge = screen.getAllByTestId('source-badge')[0];
      expect(badge.style.background).toBe('');
      expect(badge.style.border).toBe('');
      expect(badge.querySelector('span')!.style.background).toBe(hexToRgb(repoHue('core')));
    });
  });
});

// jsdom serialises an inline `color`/`background` as rgb().
function hexToRgb(hex: string): string {
  const h = hex.length === 4 ? '#' + [...hex.slice(1)].map(c => c + c).join('') : hex;
  const [r, g, b] = [1, 3, 5].map(i => parseInt(h.slice(i, i + 2), 16));
  return `rgb(${r}, ${g}, ${b})`;
}
