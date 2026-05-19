import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent, waitFor } from '@testing-library/react';
import { CommitDrawer } from './CommitDrawer';
import { init } from './state';
import type { AppState } from './state';

vi.mock('./api', () => ({
  api: {
    commitDetail: vi.fn().mockResolvedValue({
      commit: 'a1b2c3d', date: '2026-05-01T00:00:00Z', message: 'Test commit', operation: 'modify',
      files: [{ path: 'kb/x.md', action: 'modified', title: 'X' }],
    }),
    factCommits: vi.fn().mockResolvedValue({ entries: [] }),
  },
}));

function setup(overrides: Partial<AppState> = {}) {
  const state: AppState = {
    ...init,
    repo: 'knomit', branch: 'machine/test',
    commitDrawer: { open: true, commit: 'a1b2c3d' },
    ...overrides,
  };
  const dispatch = vi.fn();
  return { ...render(<CommitDrawer state={state} dispatch={dispatch} />), dispatch };
}

beforeEach(() => { vi.clearAllMocks(); });

describe('CommitDrawer — open/close', () => {
  it('renders nothing when drawer is closed', () => {
    const { container } = render(<CommitDrawer
      state={{ ...init, commitDrawer: { open: false } }}
      dispatch={vi.fn()}
    />);
    expect(container.querySelector('[data-testid="commit-drawer"]')).toBeNull();
  });

  it('renders when drawer is open', async () => {
    setup();
    await waitFor(() => expect(screen.getByTestId('commit-drawer')).toBeInTheDocument());
  });

  it('dispatches CLOSE_COMMIT_DRAWER on Escape', async () => {
    const { dispatch } = setup();
    await waitFor(() => screen.getByTestId('commit-drawer'));
    fireEvent.keyDown(window, { key: 'Escape' });
    expect(dispatch).toHaveBeenCalledWith({ type: 'CLOSE_COMMIT_DRAWER' });
  });

  it('dispatches CLOSE_COMMIT_DRAWER on close button click', async () => {
    const { dispatch } = setup();
    await waitFor(() => screen.getByTestId('drawer-close'));
    fireEvent.click(screen.getByTestId('drawer-close'));
    expect(dispatch).toHaveBeenCalledWith({ type: 'CLOSE_COMMIT_DRAWER' });
  });
});

describe('CommitDrawer — sections', () => {
  it('renders the op chip, commit hash, date, and message from CommitDetail', async () => {
    setup();
    await waitFor(() => expect(screen.getByTestId('drawer-op-chip').textContent).toContain('modify'));
    expect(screen.getByTestId('drawer-message').textContent).toBe('Test commit');
  });

  it('renders one row per file with the action glyph', async () => {
    setup();
    await waitFor(() => expect(screen.getAllByTestId('drawer-file-row').length).toBe(1));
    const row = screen.getByTestId('drawer-file-row');
    expect(row.textContent).toContain('~'); // modified → ~
    expect(row.textContent).toContain('X'); // title
  });
});
