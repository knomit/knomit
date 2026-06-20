import { it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { TimelineNav } from './TimelineNav';
import { api } from './api';

vi.mock('./api', async (orig) => {
  const mod = await orig<typeof import('./api')>();
  return { ...mod, api: { ...mod.api,
    factCommits: vi.fn(), commitDetail: vi.fn() } };
});

beforeEach(() => {
  (api.factCommits as any).mockResolvedValue({ entries: [
    { commit: 'newest1', date: '', message: 'latest', operation: 'update' },
    { commit: 'older22', date: '', message: 'first',  operation: 'learn' },
  ]});
  (api.commitDetail as any).mockResolvedValue({ commit: 'newest1', date: '', message: 'full commit message body',
    operation: 'update', author: { name: 'A', email: 'a@agents.knomit.io' },
    files: [{ path: 'kb/a.md', action: 'modified', title: 'A' },
            { path: 'kb/sib.md', action: 'added', title: 'Sib' }] });
});

it('scrubbing the newest row reports isLatest=true', async () => {
  const onScrub = vi.fn();
  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="newest1" onScrub={onScrub} onOpenFileAt={() => {}} />);
  await waitFor(() => screen.getByText('latest'));
  fireEvent.click(screen.getByText('latest'));
  expect(onScrub).toHaveBeenCalledWith('newest1', true);
});

it('a sibling files-affected row opens at the viewed commit', async () => {
  const onOpenFileAt = vi.fn();
  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="newest1" onScrub={() => {}} onOpenFileAt={onOpenFileAt} />);
  await waitFor(() => screen.getByText('Sib'));
  fireEvent.click(screen.getByText('Sib'));
  expect(onOpenFileAt).toHaveBeenCalledWith('kb/sib.md', 'newest1');
});

it('scrubbing a live fact to an older commit does NOT show the "no HEAD version" note', async () => {
  // newest entry has operation 'update' (not 'retract') — fact is live at HEAD.
  // activeCommit is the older commit, simulating time-travel scrubbing.
  (api.factCommits as any).mockResolvedValue({ entries: [
    { commit: 'newest1', date: '', message: 'latest', operation: 'update' },
    { commit: 'older22', date: '', message: 'first',  operation: 'learn' },
  ]});
  (api.commitDetail as any).mockResolvedValue({ commit: 'older22', date: '', message: 'old commit',
    operation: 'learn', author: { name: 'B', email: 'b@example.com' }, files: [] });

  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="older22" onScrub={() => {}} onOpenFileAt={() => {}} />);
  await waitFor(() => screen.getByText('latest'));
  expect(screen.queryByText(/no HEAD version/i)).toBeNull();
});
