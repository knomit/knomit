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

  it('renders SCRUBBED pill with 7-char hash in scrubbed mode', () => {
    setup({ mode: 'scrubbed', commit: 'b812d40abc' });
    expect(screen.getByText('SCRUBBED')).toBeInTheDocument();
    expect(screen.getByText('b812d40')).toBeInTheDocument();
  });

  it('renders DIFF pill with from..to range in diff mode', () => {
    setup({ mode: 'diff', from: 'aaa1111zzz', to: 'bbb2222zzz' });
    expect(screen.getByText('DIFF')).toBeInTheDocument();
    expect(screen.getByText('aaa1111..bbb2222')).toBeInTheDocument();
  });

  it('renders kbd hints [t] and [h]', () => {
    setup();
    expect(screen.getByText('t')).toBeInTheDocument();
    expect(screen.getByText('h')).toBeInTheDocument();
  });

  it('does not render [⌘K] palette hint', () => {
    setup();
    expect(screen.queryByText(/⌘K/)).toBeNull();
    expect(screen.queryByText('palette')).toBeNull();
  });

  it('renders kbd hints [t] and [h] in live mode', () => {
    setup({ mode: 'live' });
    // The t scrub · h now hints are always-present in the footer (shown in all modes)
    expect(screen.getByText('t')).toBeInTheDocument();
    expect(screen.getByText('h')).toBeInTheDocument();
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

  it('DIFF pill and kbd hints remain present with a long task message', () => {
    setup(
      { mode: 'diff', from: 'c4f1111aaa', to: 'c9a7222bbb' },
      { tasks: { sync: { status: 'running' as const,
          message: 'a very long task message that would overflow in a narrow bar at maximum pill width' } } },
    );
    expect(screen.getByText('DIFF')).toBeInTheDocument();
    expect(screen.getByText('c4f1111..c9a7222')).toBeInTheDocument();
    expect(screen.getByText('t')).toBeInTheDocument();
    expect(screen.getByText('h')).toBeInTheDocument();
  });
});

describe('Console — scrubbed pill descriptor', () => {
  // The old clickable hash launched the Explain overlay (OPEN_EXPLAIN). With the
  // overlay gone and the EdgesRail always visible beside an open fact, the
  // commit descriptor is now a plain, non-interactive label.
  it('renders the scrubbed commit descriptor and never dispatches OPEN_EXPLAIN', () => {
    const dispatch = vi.fn();
    render(<Console
      state={{ ...init, asOf: { mode: 'scrubbed', commit: 'b812d40' }, factPath: 'kb/x.md' }}
      dispatch={dispatch}
    />);
    expect(screen.getByText('b812d40')).toBeInTheDocument();
    expect(screen.queryByTestId('pill-commit-hash')).toBeNull();
    expect(dispatch).not.toHaveBeenCalled();
  });
});

describe('Console — scrubbed footer pill', () => {
  it('footer shows SCRUBBED with trail depth when scrubbed 2 hops deep', () => {
    // Build a 2-hop scrubbed trail using the reducer:
    //   from init → APPLY_NAV live (1 navStack entry, live)
    //   → APPLY_NAV scrubbed commit bbb1111 (2nd entry)
    //   → APPLY_NAV scrubbed commit ccc2222 (current, 3rd entry)
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
      asOf: { mode: 'scrubbed', commit: 'bbb1111bbb1111' },
    });
    state = reducer(state, {
      type: 'APPLY_NAV',
      view: 'library',
      factPath: 'kb/c.md',
      asOf: { mode: 'scrubbed', commit: 'ccc2222ccc2222' },
    });

    const dispatch = vi.fn();
    render(<Console state={state} dispatch={dispatch} />);

    expect(screen.getByText(/SCRUBBED/)).toBeInTheDocument();
    expect(screen.getByText(/trail 2 deep/i)).toBeInTheDocument();
  });
});
