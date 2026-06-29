import { it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent, within } from '@testing-library/react';
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

it('selecting the (non-active) newest row stays in history at that commit (no live demotion)', async () => {
  const onScrub = vi.fn();
  // Active row is the older one, so clicking the newest row is a selection.
  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="older22" onScrub={onScrub} onOpenFileAt={() => {}} />);
  await waitFor(() => screen.getByText('latest'));
  fireEvent.click(screen.getByText('latest'));
  // Re-selecting the newest version keeps the history excursion open at that
  // commit — exiting to live is the return-to-live control's job, never a side
  // effect of picking a version from the timeline.
  expect(onScrub).toHaveBeenCalledWith('newest1');
});

it('clicking a non-active commit selects (scrubs to) it', async () => {
  const onScrub = vi.fn();
  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="newest1" onScrub={onScrub} onOpenFileAt={() => {}} />);
  await waitFor(() => screen.getByText('first'));   // the older, non-active row
  fireEvent.click(screen.getByText('first'));
  expect(onScrub).toHaveBeenCalledWith('older22');
});

it('clicking the active commit collapses its detail; clicking again expands it (no scrub)', async () => {
  const onScrub = vi.fn();
  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="newest1" onScrub={onScrub} onOpenFileAt={() => {}} />);
  await waitFor(() => screen.getByText('Sib'));                 // detail expanded by default
  fireEvent.click(screen.getByText('latest'));                  // click active row → collapse
  await waitFor(() => expect(screen.queryByText('Sib')).toBeNull());
  fireEvent.click(screen.getByText('latest'));                  // click again → expand
  await waitFor(() => screen.getByText('Sib'));
  expect(onScrub).not.toHaveBeenCalled();                       // toggling never re-scrubs
});

it('a sibling files-affected row opens at the viewed commit', async () => {
  const onOpenFileAt = vi.fn();
  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="newest1" onScrub={() => {}} onOpenFileAt={onOpenFileAt} />);
  await waitFor(() => screen.getByText('Sib'));
  fireEvent.click(screen.getByText('Sib'));
  expect(onOpenFileAt).toHaveBeenCalledWith('kb/sib.md', 'newest1');
});

it('header exposes a return-to-live control wired to onReturnToLive', async () => {
  const onReturnToLive = vi.fn();
  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="newest1" onScrub={() => {}} onOpenFileAt={() => {}} onReturnToLive={onReturnToLive} />);
  await waitFor(() => screen.getByTestId('timeline-return-live'));
  const btn = screen.getByTestId('timeline-return-live');
  // Icon-only: a glyph, no visible text, label via the accessible name.
  expect(btn.querySelector('svg')).not.toBeNull();
  expect(btn).toHaveTextContent('');
  expect(btn).toHaveAccessibleName('Return to live');
  fireEvent.click(btn);
  expect(onReturnToLive).toHaveBeenCalled();
});

it('marks the current fact as "here" in files-affected and keeps it non-navigable', async () => {
  const onOpenFileAt = vi.fn();
  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="newest1" onScrub={() => {}} onOpenFileAt={onOpenFileAt} />);
  await waitFor(() => screen.getByText('Sib'));
  const selfRow = document.querySelector('[data-testid="timeline-file-row"][data-self="true"]') as HTMLElement;
  expect(selfRow).toBeTruthy();
  // The "you are here" marker is present...
  expect(within(selfRow).getByTestId('timeline-here-marker')).toBeTruthy();
  // ...and the current fact is not a navigation target.
  fireEvent.click(selfRow);
  expect(onOpenFileAt).not.toHaveBeenCalled();
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
