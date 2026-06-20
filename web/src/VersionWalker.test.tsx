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

it('shows position and steps to previous (older) version', async () => {
  const onScrub = vi.fn();
  render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" currentCommit="v3head" onScrub={onScrub} />);
  await waitFor(() => screen.getByText(/v3 of 3/i));
  fireEvent.click(screen.getByTestId('walker-prev'));
  expect(onScrub).toHaveBeenCalledWith('v2mid', false); // older = not latest
});

it('treats current as newest when currentCommit is not among entries (live branch-tip anchor)', async () => {
  // In LIVE mode the HEAD fact read returns as_of.commit = the branch tip,
  // which is not one of the fact's own version commits. The walker must still
  // position on the newest version and offer prev, not vanish.
  const onScrub = vi.fn();
  render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" currentCommit="branchTipNotAVersion" onScrub={onScrub} />);
  await waitFor(() => screen.getByText(/v3 of 3/i));      // newest position
  const prev = screen.getByTestId('walker-prev') as HTMLButtonElement;
  expect(prev.disabled).toBe(false);
  fireEvent.click(prev);
  expect(onScrub).toHaveBeenCalledWith('v2mid', false);   // steps to the older version
});

it('renders nothing for a single-version fact', async () => {
  (api.factCommits as any).mockResolvedValue({ entries: [{ commit: 'only', date: '', message: '' }] });
  const { container } = render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" currentCommit="only" onScrub={vi.fn()} />);
  await waitFor(() => expect(api.factCommits).toHaveBeenCalled());
  expect(container.querySelector('[data-testid="version-walker"]')).toBeNull();
});
