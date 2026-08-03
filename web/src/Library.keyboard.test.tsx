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
  const navigate = vi.fn();
  const dispatch = vi.fn();
  const state: AppState = {
    ...init,
    repo: 'knomit',
    branch: 'machine/test',
    headCommit: 'aaaaaaa',
    librarySort: 'path',
    ...overrides,
  };
  render(<Library state={state} dispatch={dispatch} navigate={navigate} />);
  return { navigate, dispatch };
}

describe('Library — keyboard gated to live mode', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('drives selection/navigation when live', async () => {
    const { dispatch } = setup({ asOf: { mode: 'live' } });
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(2));
    // ArrowDown selects the first row (a dir); ArrowRight activates it → NAVIGATE.
    fireEvent.keyDown(window, { key: 'ArrowDown' });
    fireEvent.keyDown(window, { key: 'ArrowRight' });
    expect(dispatch).toHaveBeenCalledWith(expect.objectContaining({ type: 'NAVIGATE' }));
  });

  it('ignores keys in history mode (Library is hidden behind TimelineNav)', async () => {
    const { navigate, dispatch } = setup({ asOf: { mode: 'history', commit: 'bbbbbbb' } });
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(2));
    // Arrows/Enter must be inert while the read-only history view is shown,
    // otherwise they steer the hidden selection and can navigate away from it.
    fireEvent.keyDown(window, { key: 'ArrowDown' });
    fireEvent.keyDown(window, { key: 'ArrowRight' });
    fireEvent.keyDown(window, { key: 'Enter' });
    expect(navigate).not.toHaveBeenCalled();
    expect(dispatch).not.toHaveBeenCalledWith(expect.objectContaining({ type: 'NAVIGATE' }));
    expect(dispatch).not.toHaveBeenCalledWith(expect.objectContaining({ type: 'FOCUS_RIGHT_PANEL' }));
  });
});

// The Library owns BARE arrows. Modified ones are window-level commands handled
// in App, and both handlers are on `window` — preventDefault in one does not
// stop the other. Before the modifier guard, Alt+← dispatched NAV_BACK from App
// AND GO_UP from here off the same event, moving two steps for one keypress.
describe('Library — modified arrows belong to window commands, not the list', () => {
  beforeEach(() => { vi.clearAllMocks(); });

  it('ArrowLeft still goes up a level', async () => {
    const { dispatch } = setup({
      asOf: { mode: 'live' },
      filters: [{ category: 'path', value: 'kb/sub' }],
    });
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(2));
    fireEvent.keyDown(window, { key: 'ArrowLeft' });
    expect(dispatch).toHaveBeenCalledWith({ type: 'GO_UP' });
  });

  it('does NOT go up when Alt is held — App is taking that as Back', async () => {
    const { dispatch } = setup({
      asOf: { mode: 'live' },
      filters: [{ category: 'path', value: 'kb/sub' }],
    });
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(2));
    fireEvent.keyDown(window, { key: 'ArrowLeft', altKey: true });
    expect(dispatch).not.toHaveBeenCalledWith({ type: 'GO_UP' });
  });

  it('does NOT drive the list when Meta or Ctrl is held', async () => {
    const { dispatch } = setup({ asOf: { mode: 'live' } });
    await waitFor(() => expect(screen.getAllByTestId('dir-entry').length).toBe(2));
    fireEvent.keyDown(window, { key: 'ArrowDown', metaKey: true });
    fireEvent.keyDown(window, { key: 'ArrowRight', ctrlKey: true });
    expect(dispatch).not.toHaveBeenCalledWith(expect.objectContaining({ type: 'NAVIGATE' }));
    expect(dispatch).not.toHaveBeenCalledWith(expect.objectContaining({ type: 'FOCUS_RIGHT_PANEL' }));
  });
});

