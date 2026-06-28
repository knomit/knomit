import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { Console } from './Console';
import { init, reducer } from './state';
import type { AppState, AsOf } from './state';

function setup(asOf: AsOf = init.asOf, overrides: Partial<AppState> = {}) {
  const state: AppState = { ...init, asOf, ...overrides };
  const dispatch = vi.fn();
  return { ...render(<Console state={state} dispatch={dispatch} />), dispatch, state };
}

describe('StatusFooter (collapsed Console)', () => {
  it('renders LIVE pill in live mode', () => {
    setup({ mode: 'live' });
    expect(screen.getByText('LIVE')).toBeInTheDocument();
    expect(screen.getByText('HEAD')).toBeInTheDocument();
  });

  it('renders HISTORY pill with 7-char hash in history mode', () => {
    setup({ mode: 'history', commit: 'b812d40abc' });
    expect(screen.getByText('HISTORY')).toBeInTheDocument();
    expect(screen.getByText('b812d40')).toBeInTheDocument();
  });

  it('renders DIFF pill with from..to range in diff mode', () => {
    setup({ mode: 'diff', from: 'aaa1111zzz', to: 'bbb2222zzz' });
    expect(screen.getByText('DIFF')).toBeInTheDocument();
    expect(screen.getByText('aaa1111..bbb2222')).toBeInTheDocument();
  });

  it('renders kbd hint [h] but NOT [t] (t scrub is not wired up)', () => {
    setup();
    expect(screen.getByText('h')).toBeInTheDocument();
    expect(screen.queryByText('t')).toBeNull();
  });

  it('does not render [⌘K] palette hint', () => {
    setup();
    expect(screen.queryByText(/⌘K/)).toBeNull();
    expect(screen.queryByText('palette')).toBeNull();
  });

  it('renders kbd hint [h] but NOT [t] in live mode', () => {
    setup({ mode: 'live' });
    // t scrub is deferred (App.tsx lacks the version list), so it must not appear
    expect(screen.getByText('h')).toBeInTheDocument();
    expect(screen.queryByText('t')).toBeNull();
  });

  it('diff mode does not render read-only or trail depth extras', () => {
    setup({ mode: 'diff', from: 'aaa1111zzz', to: 'bbb2222zzz' });
    expect(screen.queryByText(/read-only/i)).toBeNull();
    expect(screen.queryByText(/trail \d+ deep/i)).toBeNull();
  });

  it('clicking the bar fires CONSOLE_TOGGLE', () => {
    const { dispatch } = setup();
    fireEvent.click(screen.getByTestId('console'));
    expect(dispatch).toHaveBeenCalledWith({ type: 'CONSOLE_TOGGLE' });
  });

  it('renders error count when errors > 0', () => {
    setup(init.asOf, {
      consoleEntries: [
        { id: 1, time: 0, level: 'error' as const, message: 'boom' },
        { id: 2, time: 0, level: 'error' as const, message: 'pow' },
      ],
    });
    expect(screen.getByText('2 err')).toBeInTheDocument();
  });

  it('renders task chip when a task is running', () => {
    setup(init.asOf, { tasks: { sync: { status: 'running' as const, message: 'pulling…' } } });
    expect(screen.getByText('[sync] pulling…')).toBeInTheDocument();
  });

  it('DIFF pill and h hint remain present with a long task message; t hint is absent', () => {
    setup(
      { mode: 'diff', from: 'c4f1111aaa', to: 'c9a7222bbb' },
      { tasks: { sync: { status: 'running' as const,
          message: 'a very long task message that would overflow in a narrow bar at maximum pill width' } } },
    );
    expect(screen.getByText('DIFF')).toBeInTheDocument();
    expect(screen.getByText('c4f1111..c9a7222')).toBeInTheDocument();
    expect(screen.queryByText('t')).toBeNull();
    expect(screen.getByText('h')).toBeInTheDocument();
  });
});

describe('Console — history pill descriptor', () => {
  // The old clickable hash launched the Explain overlay (OPEN_EXPLAIN). With the
  // overlay gone and the EdgesRail always visible beside an open fact, the
  // commit descriptor is now a plain, non-interactive label.
  it('renders the history commit descriptor and never dispatches OPEN_EXPLAIN', () => {
    const dispatch = vi.fn();
    render(<Console
      state={{ ...init, asOf: { mode: 'history', commit: 'b812d40' }, factPath: 'kb/x.md' }}
      dispatch={dispatch}
    />);
    expect(screen.getByText('b812d40')).toBeInTheDocument();
    expect(screen.queryByTestId('pill-commit-hash')).toBeNull();
    expect(dispatch).not.toHaveBeenCalled();
  });
});

describe('Console — history footer pill', () => {
  it('footer shows HISTORY with trail depth when history 2 hops deep', () => {
    // Build a 2-hop history trail using the reducer:
    //   from init → APPLY_NAV live (1 navStack entry, live)
    //   → APPLY_NAV history commit bbb1111 (2nd entry)
    //   → APPLY_NAV history commit ccc2222 (current, 3rd entry)
    // selectTrail yields 3 crumbs → N = 3 - 1 = 2 → "trail 2 deep"
    let state = reducer(init, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: 'kb/a.md',
      asOf: { mode: 'live' },
    });
    state = reducer(state, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: 'kb/b.md',
      asOf: { mode: 'history', commit: 'bbb1111bbb1111' },
    });
    state = reducer(state, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: 'kb/c.md',
      asOf: { mode: 'history', commit: 'ccc2222ccc2222' },
    });

    const dispatch = vi.fn();
    render(<Console state={state} dispatch={dispatch} />);

    expect(screen.getByText(/HISTORY/)).toBeInTheDocument();
    expect(screen.getByText(/trail 2 deep/i)).toBeInTheDocument();
  });
});

describe('build version in the Console chrome', () => {
  it('renders the version inside the collapsed status bar (not a floating overlay)', () => {
    const dispatch = vi.fn();
    render(<Console state={init} dispatch={dispatch} version="0.5.6.8a0f0e44" />);

    const badge = screen.getByTestId('version-badge');
    expect(badge).toHaveTextContent('v0.5.6.8a0f0e44');
    // Regression: the version must live within the console bar, never overlap it.
    expect(screen.getByTestId('console')).toContainElement(badge);
  });

  it('renders nothing version-related when no version is provided', () => {
    const dispatch = vi.fn();
    render(<Console state={init} dispatch={dispatch} />);
    expect(screen.queryByTestId('version-badge')).toBeNull();
  });

  it('also shows the version in the expanded console header', () => {
    const dispatch = vi.fn();
    const state: AppState = { ...init, consoleOpen: true };
    render(<Console state={state} dispatch={dispatch} version="0.5.6.8a0f0e44" />);

    const badge = screen.getByTestId('version-badge');
    expect(badge).toHaveTextContent('v0.5.6.8a0f0e44');
    expect(screen.getByTestId('console')).toContainElement(badge);
  });
});
