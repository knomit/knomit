import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { ExperimentConflictError } from './api';
import type { ExperimentInfo } from './api';
import { ExperimentBand } from './ExperimentBand';

const recent = vi.fn();
const experimentAction = vi.fn();

vi.mock('./api', async importOriginal => {
  const actual = await importOriginal<typeof import('./api')>();
  return {
    ...actual,
    api: {
      recent: (...a: unknown[]) => recent(...a),
      experimentAction: (...a: unknown[]) => experimentAction(...a),
    },
  };
});

const exp: ExperimentInfo = {
  name: 'pr-237-experiments-ui',
  parent: 'mindev',
  fork_commit: 'f0f0f0f0',
  created_at: new Date(Date.now() - 2 * 3600_000).toISOString(),
  expires_at: new Date(Date.now() + 28 * 86_400_000).toISOString(),
};

function mount(overrides: Partial<ExperimentInfo> = {}, onEnterBranch = vi.fn()) {
  render(
    <ExperimentBand
      repo="alpha"
      branch="exp/pr-237-experiments-ui"
      experiment={{ ...exp, ...overrides }}
      onEnterBranch={onEnterBranch}
    />,
  );
  return onEnterBranch;
}

beforeEach(() => {
  recent.mockReset().mockResolvedValue({ facts: [], total: 3 });
  experimentAction.mockReset().mockResolvedValue(undefined);
});

describe('ExperimentBand', () => {
  it('counts changed facts through the SAME since-fork filter the list uses', async () => {
    mount();
    await screen.findByText('3 facts changed since the fork');
    // Falsifiable about HOW the count is obtained: a band that counted by any
    // other route (a timestamp scan, the whole corpus) would render the same
    // text off a different request. since_fork must be on, and the page size
    // must be 1 — only the total is wanted.
    expect(recent).toHaveBeenCalledWith('alpha', 'exp/pr-237-experiments-ui', '', '', 1, 0, { sinceFork: true });
  });

  it('states the expiry CONDITION when there is one', async () => {
    mount();
    expect((await screen.findByTestId('experiment-band-expiry')).textContent)
      .toBe('expires in 28 days without a commit');
  });

  it('omits the expiry segment entirely when the server sent no expires_at', async () => {
    // Absence means expiry is disabled server-side. The band must say nothing
    // rather than invent a date or print "never" on every screen.
    mount({ expires_at: undefined });
    await screen.findByTestId('experiment-band');
    expect(screen.queryByTestId('experiment-band-expiry')).toBeNull();
  });

  it('leaves for the parent branch after a commit, because the experiment is gone', async () => {
    const onEnter = mount();
    await screen.findByTestId('experiment-band');
    fireEvent.click(screen.getByTestId('experiment-band-commit'));
    await waitFor(() => expect(onEnter).toHaveBeenCalledWith('mindev'));
    expect(experimentAction).toHaveBeenCalledWith('alpha', 'pr-237-experiments-ui', 'commit');
  });

  it('opens the refusal dialog with the conflicting paths on a refused commit', async () => {
    const onEnter = mount();
    await screen.findByTestId('experiment-band');
    experimentAction.mockRejectedValueOnce(new ExperimentConflictError('refused', [
      'kb/principles/philosophy/epistemic-vs-pragmatic/3b1d9e40.md',
      'kb/decisions/mcp/learn/same-subject-refusal/7f0c2a11.md',
    ]));
    fireEvent.click(screen.getByTestId('experiment-band-commit'));

    const dialog = await screen.findByTestId('experiment-conflict-dialog');
    expect(dialog.textContent).toContain('2 facts changed on both sides');
    const paths = await screen.findByTestId('experiment-conflict-paths');
    expect(paths.textContent).toContain('epistemic-vs-pragmatic/3b1d9e40.md');
    expect(paths.textContent).toContain('same-subject-refusal/7f0c2a11.md');
    // A refused commit changed NOTHING, so the window must still be inside the
    // experiment: being moved out would imply the merge happened.
    expect(onEnter).not.toHaveBeenCalled();
    // The dialog offers NO repair (user ruling): it names who resolves the
    // conflict and can otherwise only discard. A Sync button here would
    // overwrite the experiment's version of these very facts.
    expect(screen.queryByTestId('experiment-conflict-sync')).toBeNull();
    expect(dialog.textContent).toContain('knomit_experiment');
    expect(screen.getByTestId('experiment-conflict-rollback')).toBeTruthy();
  });

  it('keeps the band usable when the count cannot be read', async () => {
    recent.mockRejectedValue(new Error('offline'));
    mount();
    // The actions are the point of the band; a failed decoration must not take
    // them down with it.
    expect(await screen.findByTestId('experiment-band-commit')).toBeTruthy();
    expect(screen.queryByTestId('experiment-band-changed')).toBeNull();
  });
});
