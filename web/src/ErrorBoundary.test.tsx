import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { ErrorBoundary } from './ErrorBoundary';

function Boom(): React.ReactElement {
  throw new Error('boom-xyz');
}

describe('ErrorBoundary', () => {
  afterEach(() => vi.restoreAllMocks());

  it('renders a dismissible fallback on a child render error and calls onReset', () => {
    // React logs the caught error; silence it to keep test output clean.
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const onReset = vi.fn();

    render(<ErrorBoundary onReset={onReset}><Boom /></ErrorBoundary>);

    // Fallback shows the error message instead of crashing the tree.
    expect(screen.getByText('boom-xyz')).toBeInTheDocument();

    fireEvent.click(screen.getByText('Dismiss'));
    expect(onReset).toHaveBeenCalled();
  });

  it('renders children normally when there is no error', () => {
    render(<ErrorBoundary><div>all good</div></ErrorBoundary>);
    expect(screen.getByText('all good')).toBeInTheDocument();
  });
});
