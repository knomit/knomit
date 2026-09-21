import { describe, it, expect, vi, beforeEach } from 'vitest';
import { useReducer } from 'react';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { Library } from './Library';
import { FilterBar } from './FilterBar';
import { ExperimentBand } from './ExperimentBand';
import { init, reducer } from './state';
import type { AppState } from './state';

// The since-fork toggle is the one control whose whole job is to change a
// REQUEST. jsdom can see the chip turn green while the list below it still
// shows the whole corpus, so a render assertion here would pass on the broken
// build — these tests assert the call, and what it carried.

const recent = vi.fn();
const search = vi.fn();
const browse = vi.fn();

vi.mock('./api', async importOriginal => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    api: {
      browse: (...a: unknown[]) => browse(...a),
      recent: (...a: unknown[]) => recent(...a),
      search: (...a: unknown[]) => search(...a),
    },
  };
});

const experiment = {
  name: 'pr-237-experiments-ui',
  parent: 'mindev',
  fork_commit: 'f0f0f0f0',
  created_at: new Date(Date.now() - 2 * 3600_000).toISOString(),
};

/** The real reducer, so the chip's dispatch has to survive it. */
function Harness({ withBand = false }: { withBand?: boolean }) {
  const [state, dispatch] = useReducer(reducer, {
    ...init,
    repo: 'alpha',
    branch: 'exp/pr-237-experiments-ui',
    headCommit: 'aaaaaaa',
    // NOT overridden: init.librarySort is 'path', which is what a reader
    // actually has when they toggle this. Forcing 'recent' here is what let
    // the bug through — the chip only ever worked in a mode nobody was in.
    experiment,
  } as AppState);
  return (
    <>
      <FilterBar state={state} dispatch={dispatch} embedded />
      {withBand && (
        <ExperimentBand repo={state.repo} branch={state.branch} experiment={experiment} onEnterBranch={vi.fn()} />
      )}
      <Library state={state} dispatch={dispatch} navigate={vi.fn()} />
    </>
  );
}

/** The opts bag of the most recent api.recent call. */
function lastRecentOpts() {
  const call = recent.mock.calls.at(-1);
  return (call?.[6] ?? {}) as { sinceFork?: boolean };
}

beforeEach(() => {
  recent.mockReset().mockResolvedValue({ facts: [], total: 3 });
  search.mockReset().mockResolvedValue({ results: [] });
  browse.mockReset().mockResolvedValue({ path: 'kb', children: [] });
});

describe('the since-fork toggle drives the REQUEST, not just the chip', () => {
  it('re-fetches the list with since_fork when toggled on', async () => {
    render(<Harness />);
    // The default mode is the TREE: it walks directories through api.browse
    // and cannot carry a content filter. This is the state the reader is
    // actually in when they reach for the toggle.
    await waitFor(() => expect(browse).toHaveBeenCalled());
    expect(recent).not.toHaveBeenCalled();

    fireEvent.click(screen.getByTestId('since-fork-toggle'));

    // Turning it on must BORROW the flat list and ask the facts endpoint with
    // since_fork. The bug this pins is a chip that changes colour while the
    // tree below it keeps every fact and issues no request at all.
    await waitFor(() => expect(recent).toHaveBeenCalled());
    expect(lastRecentOpts().sinceFork).toBe(true);
  });

  it('returns to the tree, unfiltered, when toggled back off', async () => {
    render(<Harness />);
    await waitFor(() => expect(browse).toHaveBeenCalled());
    fireEvent.click(screen.getByTestId('since-fork-toggle'));
    await waitFor(() => expect(lastRecentOpts().sinceFork).toBe(true));
    const treeCalls = browse.mock.calls.length;

    fireEvent.click(screen.getByTestId('since-fork-toggle'));

    // librarySort was never written, so removing the filter restores the mode
    // the reader chose rather than stranding them in the flat list.
    await waitFor(() => expect(browse.mock.calls.length).toBeGreaterThan(treeCalls));
  });

  it('marks the chip as checked, so the control agrees with the request', async () => {
    render(<Harness />);
    await waitFor(() => expect(browse).toHaveBeenCalled());
    const chip = screen.getByTestId('since-fork-toggle');
    expect(chip.getAttribute('aria-checked')).toBe('false');
    fireEvent.click(chip);
    await waitFor(() => expect(chip.getAttribute('aria-checked')).toBe('true'));
  });

  it('asks the SAME question the band counts, so the two cannot disagree', async () => {
    render(<Harness withBand />);
    await screen.findByText('3 facts changed since the fork');
    fireEvent.click(screen.getByTestId('since-fork-toggle'));
    await waitFor(() => expect(lastRecentOpts().sinceFork).toBe(true));

    // Both go through api.recent on the same repo+branch with since_fork set.
    // The band's own call is the one with a page size of 1.
    const calls = recent.mock.calls as unknown[][];
    const bandCall = calls.find(c => c[4] === 1);
    const listCall = calls.filter(c => c[4] !== 1).at(-1);
    expect(bandCall?.[0]).toBe('alpha');
    expect(bandCall?.[1]).toBe('exp/pr-237-experiments-ui');
    expect((bandCall?.[6] as { sinceFork?: boolean })?.sinceFork).toBe(true);
    expect(listCall?.[0]).toBe('alpha');
    expect(listCall?.[1]).toBe('exp/pr-237-experiments-ui');
    expect((listCall?.[6] as { sinceFork?: boolean })?.sinceFork).toBe(true);
  });
});
