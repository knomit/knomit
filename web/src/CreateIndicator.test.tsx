import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen, waitFor, fireEvent, act } from '@testing-library/react';
import { CreateIndicator } from './CreateIndicator';
import { __resetRepoCreatesForTest, runningCreates } from './useRepoCreates';
import { api } from './api';
import type { RepoCreateStatus } from './api';

function job(over: Partial<RepoCreateStatus> = {}): RepoCreateStatus {
  return { create_id: 'c1', name: 'kb', mode: 'subscribe', state: 'running', ...over };
}

describe('CreateIndicator', () => {
  beforeEach(() => { __resetRepoCreatesForTest(); });
  afterEach(() => { __resetRepoCreatesForTest(); vi.restoreAllMocks(); });

  it('shows the running count and opens the list when clicked', async () => {
    vi.spyOn(api, 'listRepoCreates').mockResolvedValue([
      job({ create_id: 'a', name: 'one' }),
      job({ create_id: 'b', name: 'two' }),
      // A finished job is in the list but is not RUNNING: the light counts
      // work in flight, not rows.
      job({ create_id: 'c', name: 'three', state: 'done' }),
    ]);
    const onOpen = vi.fn();
    render(<CreateIndicator onOpen={onOpen} />);

    const el = await screen.findByTestId('create-indicator');
    expect(el).toHaveAttribute('data-running', '2');
    expect(el).toHaveTextContent('2');
    expect(el).toHaveAttribute('aria-label', '2 creates running');

    fireEvent.click(el);
    expect(onOpen).toHaveBeenCalled();
  });

  // Nothing at zero. A permanent "0 creates" would be the top bar spending its
  // scarcest space on the answer "nothing is happening".
  it('renders nothing when no create is running', async () => {
    vi.spyOn(api, 'listRepoCreates').mockResolvedValue([job({ state: 'done' })]);
    const { container } = render(<CreateIndicator />);
    await waitFor(() => expect(api.listRepoCreates).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  it('renders nothing when the list is empty', async () => {
    vi.spyOn(api, 'listRepoCreates').mockResolvedValue([]);
    const { container } = render(<CreateIndicator />);
    await waitFor(() => expect(api.listRepoCreates).toHaveBeenCalled());
    expect(container).toBeEmptyDOMElement();
  });

  // A FAILED poll must not empty the list. Publishing [] on one dropped
  // request would make every pending row vanish and reappear, which reads to a
  // user as a create that disappeared — the exact failure this list exists to
  // prevent.
  it('keeps the last known list when a poll fails', async () => {
    const spy = vi.spyOn(api, 'listRepoCreates')
      .mockResolvedValueOnce([job({ create_id: 'a', name: 'one' })])
      .mockRejectedValue(new Error('network'));
    render(<CreateIndicator />);
    await screen.findByTestId('create-indicator');

    // Drive the second, failing poll directly rather than waiting out the
    // interval. Inside act() so any state update it DOES make is flushed —
    // without that, an implementation that wrongly published [] would leave
    // the DOM stale and this test would pass for the wrong reason.
    const { refreshRepoCreates } = await import('./useRepoCreates');
    await act(async () => { await refreshRepoCreates(); });
    expect(spy).toHaveBeenCalledTimes(2);

    expect(screen.getByTestId('create-indicator')).toHaveAttribute('data-running', '1');
  });
});

describe('runningCreates', () => {
  it('counts only jobs in the running state', () => {
    expect(runningCreates([
      job({ create_id: 'a' }),
      job({ create_id: 'b', state: 'failed' }),
      job({ create_id: 'c', state: 'done' }),
      job({ create_id: 'd' }),
    ])).toBe(2);
    expect(runningCreates([])).toBe(0);
  });
});
