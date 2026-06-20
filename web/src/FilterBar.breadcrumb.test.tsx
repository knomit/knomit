import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { FilterBar } from './FilterBar';
import { init, reducer } from './state';

describe('FilterBar — path chip breadcrumb', () => {
  it('renders each path segment and jumps to an ancestor on click', () => {
    const dispatch = vi.fn();
    const state = { ...init, filters: [{ category: 'path' as const, value: 'kb/architecture/store' }] };
    render(<FilterBar state={state} dispatch={dispatch} />);

    // All segments render
    expect(screen.getByText('kb')).toBeInTheDocument();
    expect(screen.getByText('architecture')).toBeInTheDocument();
    expect(screen.getByText('store')).toBeInTheDocument();

    // Clicking an ancestor segment dispatches ADD_FILTER with that ancestor path
    fireEvent.click(screen.getByText('architecture'));
    expect(dispatch).toHaveBeenCalledWith({
      type: 'ADD_FILTER',
      chip: { category: 'path', value: 'kb/architecture' },
    });
  });

  it('does not dispatch a path change when the deepest segment is clicked', () => {
    const dispatch = vi.fn();
    const state = { ...init, filters: [{ category: 'path' as const, value: 'kb/architecture/store' }] };
    render(<FilterBar state={state} dispatch={dispatch} />);

    fireEvent.click(screen.getByText('store'));
    expect(dispatch).not.toHaveBeenCalledWith(
      expect.objectContaining({ type: 'ADD_FILTER' }),
    );
  });
});

describe('FilterBar — history breadcrumb', () => {
  it('shows the trail breadcrumb when history', () => {
    // Build a history state with a 2-crumb trail via the reducer
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
      asOf: { mode: 'history', commit: 'bbb1111' },
    });

    render(
      <FilterBar
        state={state}
        dispatch={vi.fn()}
        onJumpTrail={vi.fn()}
        onReturnToNow={vi.fn()}
      />,
    );

    expect(screen.getByText(/return to now/i)).toBeInTheDocument();
  });
});
