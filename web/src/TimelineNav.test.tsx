import { describe, it, expect, vi, beforeEach } from 'vitest';
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

// The card starts CLOSED: this column is a list of versions first, and a merge
// commit's files-affected list is long enough to bury every other version under
// a card the reader never asked for.
it('starts with the active commit collapsed; clicking toggles it (no scrub)', async () => {
  const onScrub = vi.fn();
  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="newest1" onScrub={onScrub} onOpenFileAt={() => {}} />);
  await waitFor(() => screen.getByText('latest'));              // rows are in...
  expect(screen.queryByText('Sib')).toBeNull();                 // ...detail is not
  fireEvent.click(screen.getByText('latest'));                  // click active row → expand
  await waitFor(() => screen.getByText('Sib'));
  fireEvent.click(screen.getByText('latest'));                  // click again → collapse
  await waitFor(() => expect(screen.queryByText('Sib')).toBeNull());
  expect(onScrub).not.toHaveBeenCalled();                       // toggling never re-scrubs
});

// Scrubbing to a different version must not carry the previous row's open card
// over to the new one, or picking through the timeline re-buries the list.
it('collapses again when a different version is selected', async () => {
  const { rerender } = render(
    <TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="newest1" onScrub={() => {}} onOpenFileAt={() => {}} />);
  await waitFor(() => screen.getByText('latest'));
  fireEvent.click(screen.getByText('latest'));
  await waitFor(() => screen.getByText('Sib'));

  rerender(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="older22" onScrub={() => {}} onOpenFileAt={() => {}} />);
  await waitFor(() => expect(screen.queryByText('Sib')).toBeNull());
});

// The caret was a 9px text triangle — the smallest mark on the row, and easily
// read as punctuation. It is now the app's stroked chevron, rotated rather than
// swapped for a second glyph.
it('shows the disclosure state with a rotated chevron, not a text triangle', async () => {
  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="newest1" onScrub={() => {}} onOpenFileAt={() => {}} />);
  const caret = await waitFor(() => screen.getByTestId('timeline-detail-caret'));
  expect(caret.querySelector('svg')).not.toBeNull();
  expect(caret).toHaveTextContent('');
  expect(caret).toHaveAttribute('data-open', 'false');
  expect(caret.style.transform).toBe('rotate(-90deg)');

  fireEvent.click(screen.getByText('latest'));
  await waitFor(() => expect(caret).toHaveAttribute('data-open', 'true'));
  expect(caret.style.transform).toBe('none');
});

it('a sibling files-affected row opens at the viewed commit', async () => {
  const onOpenFileAt = vi.fn();
  render(<TimelineNav repo="r" branch="b" factPath="kb/a.md" activeCommit="newest1" onScrub={() => {}} onOpenFileAt={onOpenFileAt} />);
  await waitFor(() => screen.getByText('latest'));
  fireEvent.click(screen.getByText('latest'));                  // open the detail
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
  await waitFor(() => screen.getByText('latest'));
  fireEvent.click(screen.getByText('latest'));                  // open the detail
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

// The timeline REPLACES the library in this column, so without a back control
// here there is none at all while time-travelling — and since a reference now
// resolves at the commit it was added at, every edge hop lands in history.
// Arriving somewhere with no way back was the practical cost of that.
describe('TimelineNav — back', () => {
  const base = {
    repo: 'r', branch: 'b', factPath: 'kb/a.md', activeCommit: 'c1',
    onScrub: () => {}, onOpenFileAt: () => {},
  };

  it('fires onBack when there is somewhere to go', () => {
    const onBack = vi.fn();
    render(<TimelineNav {...base} canBack onBack={onBack} />);
    fireEvent.click(screen.getByTestId('timeline-back'));
    expect(onBack).toHaveBeenCalled();
  });

  it('renders disabled and does not fire when the stack is empty', () => {
    const onBack = vi.fn();
    render(<TimelineNav {...base} canBack={false} onBack={onBack} />);
    const btn = screen.getByTestId('timeline-back');
    expect(btn).toBeDisabled();
    fireEvent.click(btn);
    expect(onBack).not.toHaveBeenCalled();
  });

  // Same action, same position as the live library header's chevron, so the
  // control does not move when the column swaps between modes.
  it('leads the header, before the title', () => {
    render(<TimelineNav {...base} canBack onBack={() => {}} />);
    const back = screen.getByTestId('timeline-back');
    const title = screen.getByText('Timeline');
    expect(back.compareDocumentPosition(title) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
  });
});
