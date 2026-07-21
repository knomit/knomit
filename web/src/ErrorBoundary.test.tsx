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

  it('passes children through unchanged in the inline variant too', () => {
    render(<ErrorBoundary variant="inline"><div>all good</div></ErrorBoundary>);
    expect(screen.getByText('all good')).toBeInTheDocument();
    expect(screen.queryByTestId('panel-error')).toBeNull();
  });
});

// The inline variant exists so a failed PANEL can be contained without blanking
// the viewport. The overlay fallback is `position: fixed; inset: 0; zIndex:
// 2000` — wrapping a pane in it would black out the whole app for one bad fact
// body, the opposite of what panel boundaries are for.
describe('ErrorBoundary — inline variant', () => {
  afterEach(() => vi.restoreAllMocks());

  it('renders a contained card, never a fixed full-viewport overlay', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    render(<ErrorBoundary variant="inline" label="Panel died"><Boom /></ErrorBoundary>);

    const panel = screen.getByTestId('panel-error');
    expect(panel).toBeInTheDocument();
    expect(screen.getByText('Panel died')).toBeInTheDocument();
    expect(screen.getByText('boom-xyz')).toBeInTheDocument();

    // Contained: not fixed-positioned, and not the modal role the overlay uses.
    expect(panel.style.position).not.toBe('fixed');
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(panel.getAttribute('role')).toBe('alert');
  });

  it('offers Retry (re-mount the pane) before the whole-app reload', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const onReset = vi.fn();
    render(<ErrorBoundary variant="inline" onReset={onReset}><Boom /></ErrorBoundary>);

    expect(screen.queryByText('Dismiss')).toBeNull(); // that is the overlay's wording
    fireEvent.click(screen.getByText('Retry'));
    expect(onReset).toHaveBeenCalled();
  });

  it('the overlay variant is still the default (unchanged for the repo manager)', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    render(<ErrorBoundary><Boom /></ErrorBoundary>);

    expect(screen.getByRole('alertdialog')).toBeInTheDocument();
    expect(screen.queryByTestId('panel-error')).toBeNull();
  });
});
