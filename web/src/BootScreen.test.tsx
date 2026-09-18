import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent, act } from '@testing-library/react';
import { BootScreen } from './BootScreen';
import { initialBootState, bootReducer } from './boot';
import type { BootState } from './boot';

function stateAt(phase: BootState['phase'], over: Partial<BootState> = {}): BootState {
  return { ...initialBootState(0), phase, ...over };
}

describe('BootScreen', () => {
  afterEach(() => { vi.useRealTimers(); vi.restoreAllMocks(); });

  it('names the phase and the thing being opened', () => {
    render(<BootScreen boot={stateAt('opening', { target: 'alpha' })} onRetry={vi.fn()} now={() => 0} />);
    expect(screen.getByTestId('boot-screen')).toBeInTheDocument();
    expect(screen.getByTestId('boot-phase')).toHaveTextContent('Opening alpha…');
  });

  // The label's size changes with the phase and again when the elapsed
  // counter appears, so anything rendered BELOW it gets nudged on every
  // transition. Bar first, label under it: the two elements that do not change
  // size come first and stay put.
  it('renders the bar before the label, so a changing label moves nothing above it', () => {
    render(<BootScreen boot={stateAt('opening', { target: 'alpha' })} onRetry={vi.fn()} now={() => 0} />);

    const bar = screen.getByTestId('boot-bar');
    const phase = screen.getByTestId('boot-phase');
    // DOCUMENT_POSITION_FOLLOWING: phase comes after bar in document order.
    expect(bar.compareDocumentPosition(phase) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();

    // The label reserves its line, so it does not collapse to zero height
    // before the first phase text and then push the block down when it fills.
    expect((phase as HTMLElement).style.minHeight).not.toBe('');
  });

  it('drives the bar width from the phase, reaching 100% on the fast path', () => {
    const { rerender } = render(<BootScreen boot={stateAt('connecting')} onRetry={vi.fn()} now={() => 0} />);
    const early = Number(screen.getByTestId('boot-bar').getAttribute('aria-valuenow'));

    rerender(<BootScreen boot={stateAt('opening')} onRetry={vi.fn()} now={() => 0} />);
    const mid = Number(screen.getByTestId('boot-bar').getAttribute('aria-valuenow'));

    // Straight to done: the one-hop path never passes through `branch`, and
    // the bar must still reach 100 rather than stalling short of it.
    rerender(<BootScreen boot={stateAt('done')} onRetry={vi.fn()} now={() => 0} />);
    const end = Number(screen.getByTestId('boot-bar').getAttribute('aria-valuenow'));

    expect(early).toBeLessThan(mid);
    expect(mid).toBeLessThan(end);
    expect(end).toBe(100);
  });

  it('shows the elapsed counter only after 2 s', () => {
    vi.useFakeTimers();
    let clock = 0;
    render(<BootScreen boot={stateAt('opening', { target: 'alpha' })} onRetry={vi.fn()} now={() => clock} />);

    // Below the threshold the number is noise, so it is absent.
    expect(screen.queryByTestId('boot-elapsed')).toBeNull();

    clock = 2000;
    act(() => { vi.advanceTimersByTime(1000); });
    expect(screen.getByTestId('boot-elapsed')).toHaveTextContent('2 s');

    clock = 12_000;
    act(() => { vi.advanceTimersByTime(1000); });
    expect(screen.getByTestId('boot-elapsed')).toHaveTextContent('12 s');
  });

  it('surfaces retry feedback that used to reach only the Console', () => {
    let s = initialBootState(0);
    s = bootReducer(s, { type: 'PHASE', phase: 'opening', target: 'alpha' });
    s = bootReducer(s, { type: 'ATTEMPT_FAILED', error: 'network down' });
    s = bootReducer(s, { type: 'ATTEMPT_FAILED', error: 'network down' });

    render(<BootScreen boot={s} onRetry={vi.fn()} now={() => 0} />);

    const attempt = screen.getByTestId('boot-attempt');
    expect(attempt).toHaveTextContent('retrying (attempt 3)');
    expect(attempt).toHaveTextContent('network down');
    // Retrying is not failing: no terminal state, no Retry button yet.
    expect(screen.queryByTestId('boot-retry')).toBeNull();
  });

  it('renders the terminal error state with a working Retry', () => {
    const onRetry = vi.fn();
    const s = stateAt('opening', { target: 'alpha', failed: true, lastError: 'gave up after 6 attempts' });

    render(<BootScreen boot={s} onRetry={onRetry} now={() => 0} />);

    expect(screen.getByTestId('boot-phase')).toHaveTextContent('Could not open alpha');
    expect(screen.getByTestId('boot-error')).toHaveTextContent('gave up after 6 attempts');
    // The elapsed counter is suppressed once it has failed: the number stops
    // meaning "still working".
    expect(screen.queryByTestId('boot-elapsed')).toBeNull();

    fireEvent.click(screen.getByTestId('boot-retry'));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });

  it('keeps warning styling for failures only', () => {
    const { rerender } = render(
      <BootScreen boot={stateAt('opening', { target: 'alpha', attempt: 2, lastError: 'blip' })} onRetry={vi.fn()} now={() => 0} />,
    );
    const calmPhase = getComputedStyle(screen.getByTestId('boot-phase')).color;
    const calmBar = getComputedStyle(screen.getByTestId('boot-bar').firstElementChild as Element).background;

    rerender(<BootScreen boot={stateAt('opening', { target: 'alpha', failed: true })} onRetry={vi.fn()} now={() => 0} />);
    const failedPhase = getComputedStyle(screen.getByTestId('boot-phase')).color;
    const failedBar = getComputedStyle(screen.getByTestId('boot-bar').firstElementChild as Element).background;

    // A slow boot that is still retrying must not look like a broken one.
    expect(calmPhase).not.toBe(failedPhase);
    expect(calmBar).not.toBe(failedBar);
  });
});
