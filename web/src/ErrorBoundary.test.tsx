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

    // Contained: the fallback ROOT carries none of the overlay's viewport-owning
    // rules, and it is not the modal role. Asserted on the root's OWN style — an
    // earlier version of this check searched descendants, where the overlay's
    // fixed element could never be found because it IS the root.
    expect(panel.style.position).not.toBe('fixed');
    expect(panel.style.inset).toBe('');
    expect(panel.style.zIndex).toBe('');
    expect(screen.queryByRole('alertdialog')).toBeNull();
    expect(panel.getAttribute('role')).toBe('alert');
  });

  // Negative control for the assertion above: the overlay root really does carry
  // the rules the inline root must not, so "not fixed / no inset / no z-index"
  // discriminates between the two variants instead of being true by
  // construction.
  it('the overlay root carries exactly the viewport-owning rules the inline root must not', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    render(<ErrorBoundary label="Modal died"><Boom /></ErrorBoundary>);

    const dialog = screen.getByRole('alertdialog');
    expect(dialog.style.position).toBe('fixed');
    expect(dialog.style.inset).toBe('0px');
    expect(dialog.style.zIndex).toBe('2000');
  });

  // Fix 1. `flex: 1` on the fallback root split its COLUMN parent 50/50 with the
  // parent's other `flex: 1` child, so a crashed 40px TopBar took half the
  // viewport and a crashed 26px collapsed Console squeezed the fact list — the
  // opposite of containment. jsdom does not lay out, so this pins the CAUSE: the
  // fallback root must assert no growth of its own and must not claim the full
  // height of a content-sized slot.
  it('the fallback root asserts no flex growth and no full-height claim', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    render(<ErrorBoundary variant="inline"><Boom /></ErrorBoundary>);

    const panel = screen.getByTestId('panel-error');
    expect(panel.style.flex).toBe('');
    expect(panel.style.flexGrow).toBe('');
    expect(panel.style.height).toBe('');
    // It still fills the width it is given and stays inside its pane vertically.
    expect(panel.style.width).toBe('100%');
    expect(panel.style.maxHeight).toBe('100%');
  });

  it('offers Retry (re-mount the pane) before the whole-app reload', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    const onReset = vi.fn();
    render(<ErrorBoundary variant="inline" onReset={onReset}><Boom /></ErrorBoundary>);

    expect(screen.queryByText('Dismiss')).toBeNull(); // that is the overlay's wording
    fireEvent.click(screen.getByText('Retry'));
    expect(onReset).toHaveBeenCalled();
  });

  // Fix 8. Clearing the error state alone re-mounts the same subtree against the
  // same props, so for a deterministic crash the next render throws again and
  // Retry is a no-op wearing a recovery's clothes. onReset is the parent's
  // promise that something will change; without it the honest affordance is the
  // reload alone.
  it('hides Retry when the parent offers no onReset to make it mean anything', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    render(<ErrorBoundary variant="inline"><Boom /></ErrorBoundary>);

    expect(screen.queryByText('Retry')).toBeNull();
    expect(screen.getByText('Reload app')).toBeInTheDocument();
  });

  it('the overlay variant is still the default (unchanged for the repo manager)', () => {
    vi.spyOn(console, 'error').mockImplementation(() => {});
    render(<ErrorBoundary><Boom /></ErrorBoundary>);

    expect(screen.getByRole('alertdialog')).toBeInTheDocument();
    expect(screen.queryByTestId('panel-error')).toBeNull();
  });
});
