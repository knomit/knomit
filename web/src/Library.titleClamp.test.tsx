import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { Library } from './Library';
import { init } from './state';

const LONG = 'x'.repeat(100) + ' tail that runs well past the cap ' + 'y'.repeat(40);
const SHORT = 'A short title';

vi.mock('./api', () => ({
  api: {
    browse: vi.fn().mockResolvedValue({
      path: 'kb',
      children: [
        { name: 'long.md', is_dir: false, type: 'decision', title: 'x'.repeat(100) + ' tail that runs well past the cap ' + 'y'.repeat(40), fullPath: 'kb/long.md' },
        { name: 'short.md', is_dir: false, type: 'decision', title: 'A short title', fullPath: 'kb/short.md' },
      ],
    }),
    recent: vi.fn().mockResolvedValue({ facts: [], total: 0 }),
    search: vi.fn().mockResolvedValue({ results: [] }),
  },
}));

describe('Library — browse list title cap', () => {
  it('shows at most 120 chars of a long title, keeps the full one in title/aria-label', async () => {
    const state = { ...init, repo: 'core', branch: 'agent/main', headCommit: 'aaaaaaa', librarySort: 'path' as const };
    render(<Library state={state} dispatch={vi.fn()} navigate={vi.fn()} />);
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(2));
    const [long, short] = screen.getAllByTestId('dir-entry');
    expect(long.textContent).toContain('x'.repeat(100) + ' tail that runs well…');
    expect(long.textContent).not.toContain('y'.repeat(40));
    expect(long.getAttribute('title')).toBe(LONG);
    expect(long.getAttribute('aria-label')).toBe(LONG);
    expect(short.textContent).toContain(SHORT);
    expect(short.getAttribute('title')).toBeNull();
  });
});
