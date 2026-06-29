import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { FilterBar } from './FilterBar';
import { init } from './state';

describe('FilterBar — search-in-flight spinner', () => {
  it('shows the spinner while a search is in flight', () => {
    const state = { ...init, freeText: 'microsoft', searching: true };
    render(<FilterBar state={state} dispatch={vi.fn()} />);
    const spinner = screen.getByTestId('search-spinner');
    expect(spinner).toBeInTheDocument();
    expect(spinner).toHaveClass('icon-spin');
    expect(spinner).toHaveAttribute('aria-label', 'Searching');
  });

  it('hides the spinner when no search is in flight', () => {
    const state = { ...init, freeText: 'microsoft', searching: false };
    render(<FilterBar state={state} dispatch={vi.fn()} />);
    expect(screen.queryByTestId('search-spinner')).toBeNull();
  });
});
