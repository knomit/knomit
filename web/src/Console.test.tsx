import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { Console } from './Console';
import { init } from './state';
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

  it('renders kbd hints [h] and [/]', () => {
    setup();
    expect(screen.getByText('h')).toBeInTheDocument();
    expect(screen.getByText('/')).toBeInTheDocument();
  });

  it('does not render [⌘K] palette hint', () => {
    setup();
    expect(screen.queryByText(/⌘K/)).toBeNull();
    expect(screen.queryByText('palette')).toBeNull();
  });

  it('does not render [t] scrub hint (deferred)', () => {
    setup();
    expect(screen.queryByText(/scrub/i)).toBeNull();
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
    expect(screen.getByText('h')).toBeInTheDocument();
    expect(screen.getByText('/')).toBeInTheDocument();
  });
});
