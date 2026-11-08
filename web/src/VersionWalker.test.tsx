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

// A jump point to history, nothing more (user, 2026-11-08): no "vN" count, the
// ↺ glyph as plain text (not the ⏱ that macOS draws as a colour emoji), named by
// aria-label and title. SABOTAGE: render a count, or drop the aria-label.
it('is a bare ↺ named "Open history", with no count', async () => {
  render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" onScrub={vi.fn()} />);
  const btn = await screen.findByTestId('version-walker');
  expect(btn.textContent).toBe('↺');
  expect(btn).not.toHaveTextContent(/v\d/);
  expect(btn).toHaveAccessibleName('Open history');
  // Pinned as attributes: title alone also names the button, so the name
  // check above would pass without the aria-label.
  expect(btn).toHaveAttribute('aria-label', 'Open history');
  expect(btn).toHaveAttribute('title', 'Open history');
});

it('opens history at the newest commit on click', async () => {
  const onScrub = vi.fn();
  render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" onScrub={onScrub} />);
  await screen.findByTestId('version-walker');
  // Single control — no prev/next stepper.
  expect(screen.queryByTestId('walker-prev')).toBeNull();
  expect(screen.queryByTestId('walker-next')).toBeNull();
  fireEvent.click(screen.getByTestId('version-walker'));
  // Opens history anchored at the newest version commit. Scrubbing always means
  // "view this version in history" — never a demotion to live.
  expect(onScrub).toHaveBeenCalledWith('v3head');
});

it('renders for a single-version fact and opens its history', async () => {
  (api.factCommits as any).mockResolvedValue({ entries: [{ commit: 'only', date: '', message: '' }] });
  const onScrub = vi.fn();
  render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" onScrub={onScrub} />);
  await waitFor(() => screen.getByTestId('version-walker'));
  fireEvent.click(screen.getByTestId('version-walker'));
  expect(onScrub).toHaveBeenCalledWith('only');
});

it('suppresses the focus ring so it does not look "selected" after click', async () => {
  render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" onScrub={vi.fn()} />);
  const btn = await screen.findByTestId('version-walker');
  expect(btn.style.outline).toBe('none');
});

it('renders nothing when the fact has no versions', async () => {
  (api.factCommits as any).mockResolvedValue({ entries: [] });
  const { container } = render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" onScrub={vi.fn()} />);
  await waitFor(() => expect(api.factCommits).toHaveBeenCalled());
  expect(container.querySelector('[data-testid="version-walker"]')).toBeNull();
});
