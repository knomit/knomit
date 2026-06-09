import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { FilterBar } from './FilterBar';
import { init } from './state';

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
