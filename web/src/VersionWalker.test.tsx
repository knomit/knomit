import { it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { VersionWalker } from './VersionWalker';
import { api } from './api';

vi.mock('./api', async (orig) => {
  const mod = await orig<typeof import('./api')>();
  return { ...mod, api: { ...mod.api, factCommits: vi.fn() } };
});

beforeEach(() => {
  (api.factCommits as any).mockResolvedValue({ entries: [
    { commit: 'v3head', date: '', message: '' },
    { commit: 'v2mid',  date: '', message: '' },
    { commit: 'v1old',  date: '', message: '' },
  ]});
});

it('shows the current version and opens history at the newest commit on click', async () => {
  const onScrub = vi.fn();
  render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" currentCommit="v3head" onScrub={onScrub} />);
  await waitFor(() => screen.getByText(/v3/i));
  // Single control — no prev/next stepper.
  expect(screen.queryByTestId('walker-prev')).toBeNull();
  expect(screen.queryByTestId('walker-next')).toBeNull();
  fireEvent.click(screen.getByTestId('version-walker'));
  // Opens history anchored at the newest version commit (isLatest=false keeps
  // the anchor in history mode rather than demoting to live).
  expect(onScrub).toHaveBeenCalledWith('v3head', false);
});

it('positions on the newest version when currentCommit is the live branch tip', async () => {
  // In LIVE mode the HEAD fact read returns as_of.commit = the branch tip,
  // which is not one of the fact's own version commits. The control must still
  // label the newest version and open history at it.
  const onScrub = vi.fn();
  render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" currentCommit="branchTipNotAVersion" onScrub={onScrub} />);
  await waitFor(() => screen.getByText(/v3/i));   // newest position
  fireEvent.click(screen.getByTestId('version-walker'));
  expect(onScrub).toHaveBeenCalledWith('v3head', false);
});

it('renders for a single-version fact and opens its history', async () => {
  (api.factCommits as any).mockResolvedValue({ entries: [{ commit: 'only', date: '', message: '' }] });
  const onScrub = vi.fn();
  render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" currentCommit="only" onScrub={onScrub} />);
  await waitFor(() => screen.getByTestId('version-walker'));
  fireEvent.click(screen.getByTestId('version-walker'));
  expect(onScrub).toHaveBeenCalledWith('only', false);
});

it('renders nothing when the fact has no versions', async () => {
  (api.factCommits as any).mockResolvedValue({ entries: [] });
  const { container } = render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" currentCommit="x" onScrub={vi.fn()} />);
  await waitFor(() => expect(api.factCommits).toHaveBeenCalled());
  expect(container.querySelector('[data-testid="version-walker"]')).toBeNull();
});
