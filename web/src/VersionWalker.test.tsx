import { it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, fireEvent } from '@testing-library/react';
import { VersionWalker } from './VersionWalker';
import { api } from './api';
import type { Fact } from './api';
import { FactActions } from './FactChrome';
import { computed } from './cascade.testutil';
import { readFileSync } from 'node:fs';

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

// A jump point to history, nothing more (user, 2026-11-08): no "vN" count, a
// history (clock-rewind) icon at the newspaper glyph's size and stroke weight
// (chat review, 2026-11-08: the 10px text ↺ read as a small hook), named by
// aria-label and title. SABOTAGE: render a count or the old ↺ text, shrink the
// icon, or drop the aria-label.
it('is a bare history icon named "Open history", the newspaper glyph\'s size and weight, with no count', async () => {
  render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" onScrub={vi.fn()} />);
  const btn = await screen.findByTestId('version-walker');
  expect(btn.textContent).toBe('');
  const svg = btn.querySelector('svg')!;
  expect(svg).not.toBeNull();
  expect(svg.getAttribute('width')).toBe('15');
  expect(svg.getAttribute('height')).toBe('15');
  expect(svg.getAttribute('viewBox')).toBe('0 0 24 24');
  expect(svg.getAttribute('stroke-width')).toBe('2');
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

// A mouse press draws no ring (it would look "selected" after a click), but the
// keyboard's :focus-visible does: .k-bare's pair of rules, as on Materialize.
// SABOTAGE: drop k-bare, or restore an inline `outline: none` -> red.
it('draws no focus ring for the mouse, and the app\'s ring for keyboard focus', async () => {
  render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" onScrub={vi.fn()} />);
  const btn = await screen.findByTestId('version-walker');
  expect(btn.classList.contains('k-bare')).toBe(true);
  expect(btn.style.outline).toBe('');
  const css = readFileSync('src/App.css', 'utf8');
  expect(css).toMatch(/\.k-bare:focus \{ outline: none; \}/);
  expect(css).toMatch(/\.k-bare:focus-visible \{ outline: 2px solid #4a7ab5;/);
});

// Chat review 2, 2026-11-09: the history icon sat in the accent green at rest.
// It is styled EXACTLY as the Materialize button beside it: muted at rest, the
// accent only on hover, both from the one .k-materialize rule. Pinned against
// the real Materialize button in the same FactActions, so the two cannot drift.
// SABOTAGE: restore the inline `color: '#7c9'`, drop k-materialize, or change
// HISTORY_REST -> red.
it('rests in the Materialize button\'s muted colour and takes the accent only on hover, by the same rule', async () => {
  const fact = { path: 'kb/a.md', title: 't', type: 'observation', body: '', domain: [], entities: [], refs: [], confidence: 1, sources: 1, commit_hash: 'abc1234', commit_date: '' } as unknown as Fact;
  render(<FactActions fact={fact} histAnchor={{ repo: 'r', branch: 'b', path: 'kb/a.md' }} onScrub={vi.fn()} onMaterialize={vi.fn()} />);
  const hist = await screen.findByTestId('version-walker');
  const mat = screen.getByTestId('materialize-btn');
  for (const cls of ['k-bare', 'k-materialize']) {
    expect(hist.classList.contains(cls)).toBe(true);
    expect(mat.classList.contains(cls)).toBe(true);
  }
  // No inline colour on either: it would outrank the :hover rule.
  expect(hist.style.color).toBe('');
  // At rest: the cascade gives both the rule's var(), and both set it alike.
  expect(computed(hist, 'color', 1280)).toBe('var(--k-materialize-rest, #888)');
  expect(computed(hist, 'color', 1280)).toBe(computed(mat, 'color', 1280));
  const rest = hist.style.getPropertyValue('--k-materialize-rest');
  expect(rest).toBe(mat.style.getPropertyValue('--k-materialize-rest'));
  expect(rest).toBe('#8a96a8');
  // The opacity lift on hover is Materialize's too.
  expect(hist.style.opacity).toBe(mat.style.opacity);
  fireEvent.mouseEnter(hist);
  fireEvent.mouseEnter(mat);
  expect(hist.style.opacity).toBe('1');
  expect(mat.style.opacity).toBe('1');
  fireEvent.mouseLeave(hist);
  expect(hist.style.opacity).toBe('0.75');
  // Hover's colour is the accent; jsdom matches no :hover, so read the rule.
  const css = readFileSync('src/App.css', 'utf8');
  expect(css).toMatch(/\.k-materialize:not\(:disabled\):hover\s*\{\s*color:\s*#7c9;\s*\}/);
  expect(hist.querySelector('svg')?.getAttribute('stroke')).toBe('currentColor');
});

it('renders nothing when the fact has no versions', async () => {
  (api.factCommits as any).mockResolvedValue({ entries: [] });
  const { container } = render(<VersionWalker repo="r" branch="b" factPath="kb/a.md" onScrub={vi.fn()} />);
  await waitFor(() => expect(api.factCommits).toHaveBeenCalled());
  expect(container.querySelector('[data-testid="version-walker"]')).toBeNull();
});
