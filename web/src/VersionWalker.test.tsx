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
  await waitFor(() => screen.getByText(/v1 of 3/i));
  fireEvent.click(screen.getByTestId('walker-prev'));
  expect(onScrub).toHaveBeenCalledWith('v2mid', false); // older = not latest
});
