import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
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
