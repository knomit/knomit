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
